package configsync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	sqlrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/routing"
	sqlsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/settings"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

const systemTenant = "00000000-0000-0000-0000-000000000001"

// TestPostgresConfigWritesAreVersionedAndAnnouncedOnlyOnCommit runs two real
// PostgreSQL coordinators: node A writes, node B listens. It checks the
// compare-and-swap semantics on PostgreSQL and that an event reaches the
// other node exactly when its write commits.
func TestPostgresConfigWritesAreVersionedAndAnnouncedOnlyOnCommit(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	db := usage.RuntimeDB()
	ctx := context.Background()
	prefix := fmt.Sprintf("cstest-%d-", time.Now().UnixNano())
	// Registered first, so it runs after the coordinators have closed; other
	// packages' tests expect no stray membership rows.
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM runtime_settings WHERE setting_key LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM config_versions WHERE domain LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM cluster_nodes WHERE node_id LIKE $1`, prefix+"%")
	})

	start := func(name string) *cluster.Coordinator {
		startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		coord, err := cluster.Start(startCtx, cluster.Options{Enabled: true, NodeID: prefix + name, DSN: dsn, DB: db, Version: "test"})
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(coord.Close)
		return coord
	}
	nodeB := start("b")
	nodeA := start("a")
	t.Cleanup(func() { cluster.SetDefault(nil) })
	cluster.SetDefault(nodeA)

	received := make(chan cluster.ConfigEvent, 16)
	nodeB.Subscribe(cluster.TopicConfig, func(ev cluster.Event) {
		var payload cluster.ConfigEvent
		if ev.Resync || ev.Decode(&payload) != nil {
			return
		}
		if strings.HasPrefix(payload.Key, prefix) || strings.HasPrefix(payload.Domain, prefix) {
			received <- payload
		}
	})
	next := func(what string) cluster.ConfigEvent {
		t.Helper()
		select {
		case ev := <-received:
			return ev
		case <-time.After(10 * time.Second):
			t.Fatalf("node B did not receive %s", what)
		}
		return cluster.ConfigEvent{}
	}

	// runtime_settings: create-only, checked update, stale update.
	store := sqlsettings.NewRuntimeSettingsStore(db)
	key := prefix + "setting"
	versions, err := store.CompareAndSwap(ctx, []sqlsettings.Change{{Key: key, Value: true, Expected: 0}})
	if err != nil || versions[key] != 1 {
		t.Fatalf("create = %v, %v", versions, err)
	}
	if ev := next("the created setting"); ev.Domain != configsync.DomainRuntimeSettings || ev.Key != key || ev.Version != 1 || ev.TenantID != systemTenant {
		t.Fatalf("event = %#v", ev)
	}
	if _, err := store.CompareAndSwap(ctx, []sqlsettings.Change{{Key: key, Value: false, Expected: 0}}); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("second create err = %v, want conflict", err)
	}
	if versions, err = store.CompareAndSwap(ctx, []sqlsettings.Change{{Key: key, Value: false, Expected: 1}}); err != nil || versions[key] != 2 {
		t.Fatalf("checked update = %v, %v", versions, err)
	}
	if ev := next("the update"); ev.Version != 2 {
		t.Fatalf("update event = %#v", ev)
	}

	// A transaction that announces a change and then rolls back announces
	// nothing: the marker committed afterwards is the next thing node B sees.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configsync.PublishTx(ctx, tx, configsync.KeyEvent(configsync.DomainRuntimeSettings, "", prefix+"rolled-back", 99)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The stale write above failed and rolled back as well.
	if _, err := store.CompareAndSwap(ctx, []sqlsettings.Change{{Key: key, Value: true, Expected: 1}}); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("stale update err = %v, want conflict", err)
	}
	marker := prefix + "marker"
	if _, err := store.CompareAndSwap(ctx, []sqlsettings.Change{{Key: marker, Value: 1, Expected: 0}}); err != nil {
		t.Fatal(err)
	}
	if ev := next("the marker"); ev.Key != marker {
		t.Fatalf("after a rollback node B received %#v before the marker", ev)
	}

	// routing_config versions on PostgreSQL.
	routing := sqlrouting.NewStore(db)
	original, originalVersion := routing.GetWithVersion()
	t.Cleanup(func() {
		if original != nil {
			_, _ = routing.CompareAndSwap(ctx, *original, configsync.AnyVersion)
		} else {
			_, _ = db.Exec(`DELETE FROM routing_config WHERE tenant_id = $1 AND id = 1`, systemTenant)
		}
	})
	updated := config.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}
	newVersion, err := routing.CompareAndSwap(ctx, updated, originalVersion)
	if err != nil {
		t.Fatalf("routing CAS at %d: %v", originalVersion, err)
	}
	if _, err := routing.CompareAndSwap(ctx, updated, originalVersion); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("stale routing CAS err = %v, want conflict", err)
	}
	if stored, version := routing.GetWithVersion(); stored == nil || stored.Strategy != "fill-first" || version != newVersion {
		t.Fatalf("routing = %#v at %d, want fill-first at %d", stored, version, newVersion)
	}

	// Collection versions: the event carries the new version, a stale
	// replacement is refused.
	domain := prefix + "collection"
	if v, err := configsync.WriteCollection(ctx, db, domain, "", 0, nil); err != nil || v != 1 {
		t.Fatalf("first collection write = %d, %v", v, err)
	}
	if ev := next("the collection write"); ev.Domain != domain || ev.Version != 1 {
		t.Fatalf("collection event = %#v", ev)
	}
	if _, err := configsync.WriteCollection(ctx, db, domain, "", 0, nil); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("stale collection write err = %v, want conflict", err)
	}

	// Fingerprints read on PostgreSQL see the new versions.
	fingerprints, err := configsync.SnapshotFingerprints(ctx, db)
	if err != nil {
		t.Fatalf("SnapshotFingerprints: %v", err)
	}
	if got := fingerprints[configsync.FingerprintKey{Domain: configsync.DomainRuntimeSettings, TenantID: systemTenant, Key: key}]; got != "2" {
		t.Fatalf("fingerprint of %s = %q, want 2", key, got)
	}
	if got := fingerprints[configsync.FingerprintKey{Domain: domain, TenantID: systemTenant}]; got != "1" {
		t.Fatalf("fingerprint of %s = %q, want 1", domain, got)
	}
}

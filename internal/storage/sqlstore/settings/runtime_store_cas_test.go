package settings

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	_ "modernc.org/sqlite"
)

func openStore(t *testing.T) (RuntimeSettingsStore, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	InitRuntimeSettingsTable(db)
	return NewRuntimeSettingsStore(db), db
}

type eventLog struct {
	mu     sync.Mutex
	events []cluster.ConfigEvent
}

func (l *eventLog) install(t *testing.T) {
	t.Helper()
	restore := configsync.SetPublisherForTest(func(_ context.Context, tx *sql.Tx, ev cluster.ConfigEvent) error {
		if tx == nil {
			return errors.New("published outside the write transaction")
		}
		l.mu.Lock()
		l.events = append(l.events, ev)
		l.mu.Unlock()
		return nil
	})
	t.Cleanup(restore)
}

func (l *eventLog) snapshot() []cluster.ConfigEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]cluster.ConfigEvent(nil), l.events...)
}

func TestCompareAndSwapVersionsAndAnnounces(t *testing.T) {
	store, _ := openStore(t)
	events := &eventLog{}
	events.install(t)
	ctx := context.Background()

	versions, err := store.CompareAndSwap(ctx, []Change{{Key: "debug", Value: true, Expected: 0}})
	if err != nil || versions["debug"] != 1 {
		t.Fatalf("create = %v, %v; want version 1", versions, err)
	}
	if _, err := store.CompareAndSwap(ctx, []Change{{Key: "debug", Value: false, Expected: 0}}); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("second create err = %v, want conflict", err)
	}
	if versions, err = store.CompareAndSwap(ctx, []Change{{Key: "debug", Value: false, Expected: 1}}); err != nil || versions["debug"] != 2 {
		t.Fatalf("update = %v, %v; want version 2", versions, err)
	}
	// A stale write loses and changes nothing, including the other key of the
	// same transaction.
	_, err = store.CompareAndSwap(ctx, []Change{
		{Key: "request-retry", Value: 5, Expected: 0},
		{Key: "debug", Value: true, Expected: 1},
	})
	var conflict *configsync.ConflictError
	if !errors.As(err, &conflict) || conflict.Key != "debug" || conflict.Current != 2 {
		t.Fatalf("stale write err = %#v, want conflict on debug at 2", err)
	}
	if _, ok := store.Load("request-retry"); ok {
		t.Fatal("rolled back transaction left request-retry behind")
	}
	if err := store.Upsert("debug", true); err != nil {
		t.Fatal(err)
	}
	if entry, _ := store.Load("debug"); entry.Version != 3 || string(entry.Payload) != "true" {
		t.Fatalf("after unchecked upsert = %+v, want version 3 value true", entry)
	}

	got := events.snapshot()
	if len(got) != 3 {
		t.Fatalf("events = %#v, want one per committed write", got)
	}
	for i, want := range []int64{1, 2, 3} {
		if got[i].Domain != configsync.DomainRuntimeSettings || got[i].Key != "debug" || got[i].Version != want {
			t.Fatalf("event %d = %#v", i, got[i])
		}
	}
}

func TestCommitChangesWritesOnlyEditedKeys(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	if _, err := store.CompareAndSwap(ctx, []Change{
		{Key: runtimeconfig.RuntimeSettingDebug, Value: false, Expected: 0},
		{Key: runtimeconfig.RuntimeSettingRequestRetry, Value: 1, Expected: 0},
	}); err != nil {
		t.Fatal(err)
	}

	// Node A and node B load the same stored values.
	nodeA, nodeB := &config.Config{}, &config.Config{}
	for _, cfg := range []*config.Config{nodeA, nodeB} {
		store.ApplyToConfigRecording(cfg, cfg.RuntimeSettingState())
		Rebaseline(cfg)
	}

	// A changes debug; B, unaware, changes request-retry. Neither save may
	// touch the key the other node owns.
	nodeA.Debug = true
	if keys, err := CommitChanges(ctx, store, nodeA, nil); err != nil || len(keys) != 1 || keys[0] != runtimeconfig.RuntimeSettingDebug {
		t.Fatalf("node A commit = %v, %v", keys, err)
	}
	nodeB.RequestRetry = 4
	if keys, err := CommitChanges(ctx, store, nodeB, nil); err != nil || len(keys) != 1 || keys[0] != runtimeconfig.RuntimeSettingRequestRetry {
		t.Fatalf("node B commit = %v, %v (stale debug must not be written back)", keys, err)
	}
	if entry, _ := store.Load(runtimeconfig.RuntimeSettingDebug); string(entry.Payload) != "true" {
		t.Fatalf("debug = %s, node B reverted node A's change", entry.Payload)
	}

	// Both nodes now edit debug from the same loaded version: the later
	// commit is rejected instead of silently overwriting.
	nodeB.Debug = false
	nodeB.RuntimeSettingState().Set(runtimeconfig.RuntimeSettingDebug, config.RuntimeSettingSnapshot{Canonical: []byte("true"), Version: 1})
	if _, err := CommitChanges(ctx, store, nodeB, nil); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("stale debug commit err = %v, want conflict", err)
	}
	ReloadKeys(store, nodeB, runtimeconfig.RuntimeSettingDebug)
	if !nodeB.Debug || nodeB.RuntimeSettingState().Version(runtimeconfig.RuntimeSettingDebug) != 2 {
		t.Fatalf("reload after conflict: debug=%v version=%d", nodeB.Debug, nodeB.RuntimeSettingState().Version(runtimeconfig.RuntimeSettingDebug))
	}

	// A client that names the version it edited is checked against it.
	stale := int64(1)
	nodeB.RequestRetry = 9
	if _, err := CommitChanges(ctx, store, nodeB, &stale); !errors.Is(err, configsync.ErrVersionConflict) {
		t.Fatalf("client version 1 on request-retry v2 err = %v, want conflict", err)
	}
}

func TestEnsureStoredProviderIDsIsDeterministicAcrossNodes(t *testing.T) {
	store, _ := openStore(t)
	ctx := context.Background()
	// A row written before provider IDs existed.
	if _, err := store.CompareAndSwap(ctx, []Change{{Key: runtimeconfig.RuntimeSettingGeminiKeys, Value: []config.GeminiKey{{APIKey: "gemini-key-X"}}, Expected: 0}}); err != nil {
		t.Fatal(err)
	}
	nodeA, nodeB := &config.Config{}, &config.Config{}
	for _, cfg := range []*config.Config{nodeA, nodeB} {
		store.ApplyToConfigRecording(cfg, cfg.RuntimeSettingState())
	}
	if !EnsureStoredProviderIDs(ctx, store, nodeA) || !EnsureStoredProviderIDs(ctx, store, nodeB) {
		t.Fatal("IDs were not assigned")
	}
	if nodeA.GeminiKey[0].ID == "" || nodeA.GeminiKey[0].ID != nodeB.GeminiKey[0].ID {
		t.Fatalf("nodes derived different IDs: %q vs %q", nodeA.GeminiKey[0].ID, nodeB.GeminiKey[0].ID)
	}
	entry, _ := store.Load(runtimeconfig.RuntimeSettingGeminiKeys)
	stored := &config.Config{}
	runtimeconfig.Specs()[0].Apply(stored, entry.Payload)
	if stored.GeminiKey[0].ID != nodeA.GeminiKey[0].ID {
		t.Fatalf("stored ID %q != derived %q", stored.GeminiKey[0].ID, nodeA.GeminiKey[0].ID)
	}
	// One write-back (node A); node B lost the CAS, re-read, and found the
	// same IDs already stored.
	if entry.Version != 2 {
		t.Fatalf("version after backfill = %d, want 2", entry.Version)
	}
}

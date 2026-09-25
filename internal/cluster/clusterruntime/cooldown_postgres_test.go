package clusterruntime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	postgresstore "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TestPostgresCooldownRelayAcrossNodes runs the relay over the real bus
// (LISTEN/NOTIFY) and subscribes the receiving node the way StartService
// does: on the coordinator cluster.Prepare installed, before cluster.Start
// adopts it. The joining node's membership event also delivers a Resync to
// the receiver, which must stay harmless.
func TestPostgresCooldownRelayAcrossNodes(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIRELAY_POSTGRES_TEST_DSN"))
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	ctx := context.Background()
	db, err := postgresstore.OpenRuntimeDB(ctx, config.PostgresConfig{DSN: dsn, MaxOpenConns: 8, MaxIdleConns: 2})
	if err != nil {
		t.Fatalf("open runtime db: %v", err)
	}
	prefix := fmt.Sprintf("crt-%04x-", time.Now().UnixNano()&0xffff)
	cleanup := func() { _, _ = db.Exec(`DELETE FROM cluster_nodes WHERE node_id LIKE $1`, prefix+"%") }
	cleanup()
	t.Cleanup(func() {
		cluster.SetDefault(nil)
		cleanup()
		_ = db.Close()
	})

	start := func(nodeID string) *cluster.Coordinator {
		startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := cluster.Start(startCtx, cluster.Options{Enabled: true, NodeID: nodeID, DSN: dsn, DB: db, Version: "test"})
		if err != nil {
			t.Fatalf("start %s: %v", nodeID, err)
		}
		t.Cleanup(c.Close)
		return c
	}

	// Node b: subscribe first, on the prepared coordinator, then join.
	prepared := cluster.Prepare(cluster.Options{Enabled: true, NodeID: prefix + "b", DSN: dsn})
	managerB := newManager(t, "acct-1")
	relayB := newCooldownRelay(func() *cluster.Coordinator { return prepared })
	relayB.start(prepared)
	relayB.attach(managerB)
	t.Cleanup(relayB.close)
	if coordB := start(prefix + "b"); coordB != prepared {
		t.Fatal("cluster.Start must adopt the prepared coordinator the relay subscribed on")
	}

	coordA := start(prefix + "a")
	managerA := newManager(t, "acct-1")
	relayA := newCooldownRelay(func() *cluster.Coordinator { return coordA })
	relayA.start(coordA)
	relayA.attach(managerA)
	t.Cleanup(relayA.close)

	retry := 10 * time.Minute
	managerA.MarkResult(ctx, coreauth.Result{
		AuthID: "acct-1", Provider: "codex", Model: "gpt-5.5", RetryAfter: &retry,
		Error: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota_exhausted"},
	})
	deadline := time.Now().Add(15 * time.Second)
	for !blockedFor(managerB, "acct-1", "gpt-5.5") {
		if time.Now().After(deadline) {
			t.Fatal("node b never applied node a's cooldown over the PostgreSQL bus")
		}
		time.Sleep(20 * time.Millisecond)
	}
	auth, _ := managerB.GetByID("acct-1")
	state := auth.ModelStates["gpt-5.5"]
	if !state.Quota.Exceeded || !strings.Contains(state.StatusMessage, prefix+"a") {
		t.Fatalf("relayed state = %+v", state)
	}
	if wait := time.Until(state.NextRetryAfter); wait < 9*time.Minute || wait > 10*time.Minute+time.Second {
		t.Fatalf("relayed deadline %s away, want about 10m", wait)
	}
}

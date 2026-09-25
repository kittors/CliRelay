package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// clusterTestNode is one "node" of an in-process cluster: its own server,
// config, access and credential managers, joined to a MemoryHub. The nodes
// share the SQLite runtime database the way real nodes share PostgreSQL.
type clusterTestNode struct {
	server *Server
	coord  *cluster.Coordinator
	sync   *configsync.Dispatcher

	mu            sync.Mutex
	fullReloads   int
	modelsChanged []string
}

func (n *clusterTestNode) counts() (int, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.fullReloads, append([]string(nil), n.modelsChanged...)
}

func setupClusterTestDB(t *testing.T) {
	t.Helper()
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "cluster.db"), proxyconfig.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("usage.InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	t.Cleanup(func() { cluster.SetDefault(nil) })
}

func newClusterTestNode(t *testing.T, hub *cluster.MemoryHub, id string) *clusterTestNode {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &proxyconfig.Config{AuthDir: authDir}
	cfg.Routing.IncludeDefaultGroup = true
	usage.ApplyStoredRuntimeSettings(cfg)
	usage.ApplyStoredRoutingConfig(cfg)
	usage.ApplyStoredProxyPool(cfg)

	node := &clusterTestNode{coord: hub.Join(id)}
	node.server = NewServer(cfg, auth.NewManager(nil, nil, nil), sdkaccess.NewManager(), configPath,
		WithModelConfigMutatedCallback(func(tenantID string) {
			node.mu.Lock()
			node.modelsChanged = append(node.modelsChanged, tenantID)
			node.mu.Unlock()
		}),
		WithConfigResyncCallback(func(*proxyconfig.Config) {
			node.mu.Lock()
			node.fullReloads++
			node.mu.Unlock()
		}),
	)
	node.sync = node.server.startConfigSyncWith(node.coord, 10*time.Millisecond)
	t.Cleanup(node.server.stopConfigSync)
	return node
}

// as makes node the process-wide coordinator, which is what the store write
// paths publish through, for the duration of fn.
func (n *clusterTestNode) as(fn func()) {
	cluster.SetDefault(n.coord)
	defer cluster.SetDefault(nil)
	fn()
}

func (n *clusterTestNode) waitIdle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.sync.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}

func authenticates(t *testing.T, node *clusterTestNode, key string) bool {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	_, authErr := node.server.accessManager.Authenticate(context.Background(), req)
	return authErr == nil
}

func TestClusterAPIKeyRevokedOnOneNodeIsRejectedOnTheOther(t *testing.T) {
	setupClusterTestDB(t)
	const key = "sk-cluster-test-XXXXXXXX"
	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: key, Name: "cluster"}); err != nil {
		t.Fatal(err)
	}
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := newClusterTestNode(t, hub, "a"), newClusterTestNode(t, hub, "b")
	if !authenticates(t, nodeA, key) || !authenticates(t, nodeB, key) {
		t.Fatal("both nodes must accept the key before it is revoked")
	}

	nodeA.as(func() {
		row := usage.GetAPIKey(key)
		row.Disabled = true
		if err := usage.UpdateAPIKeyByIDForTenant(identity.SystemTenantID, *row); err != nil {
			t.Fatal(err)
		}
	})
	nodeB.waitIdle(t)
	if authenticates(t, nodeB, key) {
		t.Fatal("node B still accepts a key node A disabled")
	}

	nodeA.as(func() {
		if err := usage.DeleteAPIKeyForTenant(identity.SystemTenantID, key); err != nil {
			t.Fatal(err)
		}
	})
	nodeB.waitIdle(t)
	if authenticates(t, nodeB, key) {
		t.Fatal("node B still accepts a key node A deleted")
	}

	// Negative control: a node that misses the event keeps its stale key map
	// until the Resync after its reconnect finds the change.
	const second = "sk-cluster-second-XXXXXXXX"
	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: second, Name: "second"}); err != nil {
		t.Fatal(err)
	}
	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if !authenticates(t, nodeB, second) {
		t.Fatal("node B did not pick up the new key on resync")
	}
	hub.Disconnect("b")
	nodeA.as(func() {
		if err := usage.DeleteAPIKeyForTenant(identity.SystemTenantID, second); err != nil {
			t.Fatal(err)
		}
	})
	if !authenticates(t, nodeB, second) {
		t.Fatal("control: a disconnected node cannot have heard of the deletion")
	}
	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if authenticates(t, nodeB, second) {
		t.Fatal("node B still accepts the deleted key after resync")
	}
	if full, _ := nodeB.counts(); full != 0 {
		t.Fatalf("key changes caused %d full reloads, want none", full)
	}
}

func TestClusterConfigChangesReachThePeer(t *testing.T) {
	setupClusterTestDB(t)
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := newClusterTestNode(t, hub, "a"), newClusterTestNode(t, hub, "b")
	ctx := context.Background()

	// Routing, including a strategy change that swaps the selector.
	nodeA.as(func() {
		routing := proxyconfig.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}
		if _, err := usage.CompareAndSwapRoutingConfigForTenant(ctx, identity.SystemTenantID, routing, configsync.AnyVersion); err != nil {
			t.Fatal(err)
		}
	})
	// Proxy pool.
	nodeA.as(func() {
		entries := []proxyconfig.ProxyPoolEntry{{ID: "hk", Name: "hk", URL: "http://127.0.0.1:7890", Enabled: true}}
		if _, err := usage.ReplaceProxyPoolForTenantExpect(ctx, identity.SystemTenantID, entries, configsync.AnyVersion); err != nil {
			t.Fatal(err)
		}
	})
	// A runtime setting, written the way a management save writes it.
	nodeA.as(func() {
		fresh := usage.FreshRuntimeConfig(nodeA.server.cfg, identity.SystemTenantID)
		fresh.RequestRetry = 7
		if _, err := usage.CommitRuntimeSettings(ctx, identity.SystemTenantID, fresh, nil); err != nil {
			t.Fatal(err)
		}
	})
	// A model config: the peer re-registers that tenant's models.
	nodeA.as(func() {
		if err := usage.UpsertModelConfigForTenant(identity.SystemTenantID, usage.ModelConfigRow{ModelID: "cluster-test-model", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	})
	nodeB.waitIdle(t)

	cfgB := nodeB.server.cfg
	if cfgB.Routing.Strategy != "fill-first" {
		t.Fatalf("node B routing strategy = %q, want fill-first", cfgB.Routing.Strategy)
	}
	if len(cfgB.ProxyPool) != 1 || cfgB.ProxyPool[0].URL != "http://127.0.0.1:7890" {
		t.Fatalf("node B proxy pool = %#v", cfgB.ProxyPool)
	}
	if cfgB.RequestRetry != 7 {
		t.Fatalf("node B request-retry = %d, want 7", cfgB.RequestRetry)
	}
	full, models := nodeB.counts()
	if full != 0 {
		t.Fatalf("node B ran %d full reloads, want per-domain reloads only", full)
	}
	if len(models) == 0 || models[len(models)-1] != identity.SystemTenantID {
		t.Fatalf("node B model refreshes = %v, want the system tenant", models)
	}

	// Pricing: change the row behind node B's cache, then announce it.
	if _, err := usage.RuntimeDB().Exec(`INSERT INTO model_pricing (tenant_id, model_id, input_price_per_million, output_price_per_million, cached_price_per_million, updated_at) VALUES (?, 'priced-model', 3, 4, 0, ?)`, identity.SystemTenantID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	nodeA.as(func() {
		_ = nodeA.coord.Publish(ctx, cluster.TopicConfig, configsync.Event(configsync.DomainPricing, ""))
	})
	nodeB.waitIdle(t)
	if row, ok := usage.GetModelPricing("priced-model"); !ok || row.InputPricePerMillion != 3 {
		t.Fatalf("pricing after reload = %#v (%v)", row, ok)
	}
}

func TestClusterResyncReloadsOnlyWhatChanged(t *testing.T) {
	setupClusterTestDB(t)
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := newClusterTestNode(t, hub, "a"), newClusterTestNode(t, hub, "b")
	ctx := context.Background()

	// A Resync with nothing changed (another node joining) reloads nothing.
	hub.Join("c")
	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if full, models := nodeB.counts(); full != 0 || len(models) != 0 {
		t.Fatalf("idle resync reloaded: full=%d models=%v", full, models)
	}

	// A change published while node B was cut off is found by the Resync that
	// follows its reconnect, through the fingerprints.
	hub.Disconnect("b")
	nodeA.as(func() {
		routing := proxyconfig.RoutingConfig{Strategy: "fill-first", IncludeDefaultGroup: true}
		if _, err := usage.CompareAndSwapRoutingConfigForTenant(ctx, identity.SystemTenantID, routing, configsync.AnyVersion); err != nil {
			t.Fatal(err)
		}
	})
	if nodeB.server.cfg.Routing.Strategy == "fill-first" {
		t.Fatal("node B saw a change while disconnected")
	}
	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if nodeB.server.cfg.Routing.Strategy != "fill-first" {
		t.Fatalf("node B routing after resync = %q, want fill-first", nodeB.server.cfg.Routing.Strategy)
	}
	if full, _ := nodeB.counts(); full != 0 {
		t.Fatalf("resync ran %d full reloads, want only the routing reload", full)
	}

	// Node B's own write is not reloaded again by a later Resync.
	nodeB.as(func() {
		fresh := usage.FreshRuntimeConfig(nodeB.server.cfg, identity.SystemTenantID)
		fresh.ForceModelPrefix = true
		if _, err := usage.CommitRuntimeSettings(ctx, identity.SystemTenantID, fresh, nil); err != nil {
			t.Fatal(err)
		}
	})
	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if _, models := nodeB.counts(); len(models) != 0 {
		t.Fatalf("node B reloaded its own write on resync: model refreshes %v", models)
	}
}

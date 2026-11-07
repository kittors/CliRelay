package api

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func replyOn() loopguard.Settings {
	return loopguard.Settings{Scope: loopguard.Scope{Reply: loopguard.ReplyRule{Enabled: loopguard.Bool(true)}}}
}

// The registry caches a tenant's settings for 15 seconds. Another node's change
// is announced under the loop_guard domain and drops the cached entry, so it
// applies here at once and not when the TTL runs out.
func TestClusterLoopGuardSettingsChangedElsewhereApplyHere(t *testing.T) {
	setupClusterTestDB(t)
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := newClusterTestNode(t, hub, "a"), newClusterTestNode(t, hub, "b")
	ctx := context.Background()

	registry := loopguard.Default()
	registry.SetStore(settingsstore.LoopGuardSettings{})
	t.Cleanup(func() { registry.SetStore(nil) })

	const tenant = "tenant-cluster-loop-guard"
	on := func() bool { return registry.Effective(tenant, "key-1").Reply != nil }

	// This node has read the tenant's settings, and they were empty.
	if on() {
		t.Fatal("setup: a tenant with nothing stored has the reply check on")
	}

	// A row written with no announcement is not seen: the cached entry is served.
	// This is the staleness the announcement exists to end.
	if _, err := usage.CompareAndSwapRuntimeSetting(ctx, tenant, loopguard.SettingKey, replyOn(), 0); err != nil {
		t.Fatal(err)
	}
	if on() {
		t.Fatal("setup: the cache was not serving the entry it read before the row was written")
	}

	// Announced the way a node's own write is, by the domain its key is routed to.
	nodeA.as(func() {
		if err := nodeA.coord.Publish(ctx, cluster.TopicConfig, configsync.Event(configsync.RuntimeSettingDomain(loopguard.SettingKey), tenant)); err != nil {
			t.Fatal(err)
		}
	})
	nodeB.waitIdle(t)
	if !on() {
		t.Fatal("the announcement did not drop the cached settings")
	}
	// By the domain's own handler: a node that does not know a domain falls back
	// to reloading everything, which would hide a missing handler here.
	if full, _ := nodeB.counts(); full != 0 {
		t.Fatalf("the change caused %d full reloads, want only the loop_guard reload", full)
	}

	// And a write through the store announces itself.
	nodeA.as(func() {
		stored, err := settingsstore.LoopGuardSettings{}.Load(ctx, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if err := (settingsstore.LoopGuardSettings{}).Save(ctx, tenant, loopguard.Settings{}, stored.Version); err != nil {
			t.Fatal(err)
		}
	})
	nodeB.waitIdle(t)
	if on() {
		t.Fatal("a write through the store was not announced: the cached settings outlived it")
	}
}

// A node cut off while another saved the settings finds the change when it
// reconnects, through the fingerprints, under the same domain.
func TestClusterLoopGuardSettingsMissedWhileDisconnectedApplyOnResync(t *testing.T) {
	setupClusterTestDB(t)
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := newClusterTestNode(t, hub, "a"), newClusterTestNode(t, hub, "b")
	ctx := context.Background()

	registry := loopguard.Default()
	registry.SetStore(settingsstore.LoopGuardSettings{})
	t.Cleanup(func() { registry.SetStore(nil) })

	const tenant = "tenant-cluster-loop-guard-resync"
	on := func() bool { return registry.Effective(tenant, "key-1").Tool != nil }
	if on() {
		t.Fatal("setup: nothing is stored but the tool check is on")
	}

	hub.Disconnect("b")
	nodeA.as(func() {
		settings := loopguard.Settings{Scope: loopguard.Scope{Tool: loopguard.ToolRule{Enabled: loopguard.Bool(true)}}}
		if err := (settingsstore.LoopGuardSettings{}).Save(ctx, tenant, settings, 0); err != nil {
			t.Fatal(err)
		}
	})
	if on() {
		t.Fatal("setup: the change reached a disconnected node")
	}

	hub.Reconnect("b")
	nodeB.waitIdle(t)
	if !on() {
		t.Fatal("a reconnecting node did not find the settings saved while it was away")
	}
	if full, _ := nodeB.counts(); full != 0 {
		t.Fatalf("the resync ran %d full reloads, want only the loop_guard reload", full)
	}
}

func TestReloadLoopGuardSettingsDropsOnlyTheNamedTenants(t *testing.T) {
	registry := loopguard.Default()
	store := loopguard.NewMemoryStore()
	registry.SetStore(store)
	t.Cleanup(func() { registry.SetStore(nil) })
	ctx := context.Background()

	for _, tenant := range []string{loopGuardTenantA, loopGuardTenantB} {
		if err := store.Save(ctx, tenant, replyOn(), 0); err != nil {
			t.Fatal(err)
		}
	}
	// Both are cached as stored.
	for _, tenant := range []string{loopGuardTenantA, loopGuardTenantB} {
		if registry.Effective(tenant, "k").Reply == nil {
			t.Fatalf("setup: %s not read", tenant)
		}
	}
	// Both rows change behind the cache.
	for _, tenant := range []string{loopGuardTenantA, loopGuardTenantB} {
		if err := store.Save(ctx, tenant, loopguard.Settings{}, 1); err != nil {
			t.Fatal(err)
		}
	}

	reloadLoopGuardSettings([]cluster.ConfigEvent{configsync.Event(configsync.DomainLoopGuard, loopGuardTenantA)})
	if registry.Effective(loopGuardTenantA, "k").Reply != nil {
		t.Fatal("the named tenant still serves the settings from before the change")
	}
	if registry.Effective(loopGuardTenantB, "k").Reply == nil {
		t.Fatal("a tenant the event did not name was reloaded")
	}

	// An event that names no tenant, or none at all, may concern any of them.
	reloadLoopGuardSettings([]cluster.ConfigEvent{{Domain: configsync.DomainLoopGuard}})
	if registry.Effective(loopGuardTenantB, "k").Reply != nil {
		t.Fatal("an event naming no tenant did not drop every tenant's cached settings")
	}
}

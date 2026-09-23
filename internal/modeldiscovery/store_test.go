package modeldiscovery

import (
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

func resetStore(t *testing.T) {
	t.Helper()
	ResetForTest()
	SetRoutingChangeHook(nil)
	t.Cleanup(func() {
		ResetForTest()
		SetRoutingChangeHook(nil)
	})
}

// testClock has its own lock: the store calls now() while holding mu.
type testClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *testClock) advance(by time.Duration) {
	c.mu.Lock()
	c.current = c.current.Add(by)
	c.mu.Unlock()
}

// useClock makes the store read time from the returned clock.
func useClock(t *testing.T) *testClock {
	t.Helper()
	clock := &testClock{current: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)}
	t.Cleanup(SetClockForTest(clock.now))
	return clock
}

func models(ids ...string) []*sdkmodelcatalog.ModelInfo {
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, &sdkmodelcatalog.ModelInfo{ID: id})
	}
	return out
}

func ids(list []*sdkmodelcatalog.ModelInfo) []string {
	out := make([]string, 0, len(list))
	for _, model := range list {
		out = append(out, model.ID)
	}
	return out
}

func TestSnapshotOutlivesTheFreshnessWindow(t *testing.T) {
	resetStore(t)
	clock := useClock(t)
	Store("tenant-a", "codex", models("gpt-a"))

	clock.advance(FreshFor + time.Hour)

	// Freshness only decides when to ask again; a routable model must not vanish
	// because a timer ran out.
	if fresh := Fresh("tenant-a", "codex"); len(fresh) != 0 {
		t.Fatalf("Fresh = %v after the window, want nothing", ids(fresh))
	}
	if snapshot := Snapshot("tenant-a", "codex"); len(snapshot) != 1 || snapshot[0].ID != "gpt-a" {
		t.Fatalf("Snapshot = %v, want the last stored list", ids(snapshot))
	}
}

func TestStoreIgnoresAnEmptyAnswer(t *testing.T) {
	resetStore(t)
	Store("tenant-a", "codex", models("gpt-a"))

	Store("tenant-a", "codex", nil)
	Store("tenant-a", "codex", []*sdkmodelcatalog.ModelInfo{nil, {ID: "  "}})

	if snapshot := Snapshot("tenant-a", "codex"); len(snapshot) != 1 {
		t.Fatalf("Snapshot = %v, an empty answer must not deregister everything", ids(snapshot))
	}
}

func TestListsAreScopedByTenantAndProvider(t *testing.T) {
	resetStore(t)
	Store("tenant-a", "codex", models("gpt-a"))
	Store("tenant-b", "codex", models("gpt-b"))
	Store("tenant-a", "grok", models("grok-a"))

	if got := ids(Snapshot("tenant-a", "codex")); len(got) != 1 || got[0] != "gpt-a" {
		t.Fatalf("tenant-a codex = %v", got)
	}
	if got := ids(Snapshot("tenant-b", "codex")); len(got) != 1 || got[0] != "gpt-b" {
		t.Fatalf("tenant-b codex = %v", got)
	}
	// Provider aliases share one list.
	if got := ids(Snapshot("tenant-a", "xai")); len(got) != 1 || got[0] != "grok-a" {
		t.Fatalf("tenant-a xai = %v", got)
	}
	// The empty tenant is the system tenant.
	Store("", "codex", models("gpt-system"))
	if got := ids(Snapshot(coreauth.NormalizedTenantID(""), "codex")); len(got) != 1 || got[0] != "gpt-system" {
		t.Fatalf("system tenant codex = %v", got)
	}
}

func TestRoutingHookRunsOnlyWhenTheListChanges(t *testing.T) {
	resetStore(t)
	var (
		hookMu sync.Mutex
		calls  []string
	)
	SetRoutingChangeHook(func(tenantID, provider string) {
		hookMu.Lock()
		calls = append(calls, tenantID+"|"+provider)
		hookMu.Unlock()
	})
	count := func() int {
		hookMu.Lock()
		defer hookMu.Unlock()
		return len(calls)
	}

	Store("tenant-a", "codex", models("gpt-a", "gpt-b"))
	if count() != 1 {
		t.Fatalf("first list: hook calls = %d, want 1", count())
	}
	// Same models in another order: nothing to re-register.
	Store("tenant-a", "codex", models("gpt-b", "gpt-a"))
	if count() != 1 {
		t.Fatalf("unchanged list: hook calls = %d, want 1", count())
	}
	Store("tenant-a", "codex", models("gpt-a", "gpt-b", "gpt-c"))
	if count() != 2 {
		t.Fatalf("added model: hook calls = %d, want 2", count())
	}
	changed := models("gpt-a", "gpt-b", "gpt-c")
	changed[0].Thinking = &sdkmodelcatalog.ThinkingSupport{Levels: []string{"low"}}
	Store("tenant-a", "codex", changed)
	if count() != 3 {
		t.Fatalf("changed reasoning levels: hook calls = %d, want 3", count())
	}
	// A provider that does not drive routing never asks for re-registration.
	Store("tenant-a", "xai", models("grok-a"))
	if count() != 3 {
		t.Fatalf("display-only provider: hook calls = %d, want 3", count())
	}
	if calls[0] != coreauth.NormalizedTenantID("tenant-a")+"|codex" {
		t.Fatalf("hook arguments = %q", calls[0])
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	resetStore(t)
	Store("tenant-a", "codex", []*sdkmodelcatalog.ModelInfo{{
		ID:       "gpt-a",
		Thinking: &sdkmodelcatalog.ThinkingSupport{Levels: []string{"low", "high"}},
	}})

	first := Snapshot("tenant-a", "codex")
	first[0].ID = "mutated"
	first[0].Thinking.Levels[0] = "mutated"

	second := Snapshot("tenant-a", "codex")
	if second[0].ID != "gpt-a" || second[0].Thinking.Levels[0] != "low" {
		t.Fatalf("stored list was mutated through a snapshot: %+v", second[0])
	}
}

func TestIsDiscoveryCredential(t *testing.T) {
	oauth := &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"email": "a@example.com"}}
	apiKey := &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	apiKind := &coreauth.Auth{Provider: "claude", Attributes: map[string]string{"auth_kind": "apikey"}}
	xaiKey := &coreauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "xai-test"}}

	if !IsDiscoveryCredential(oauth, "codex") {
		t.Fatal("OAuth codex credential must qualify")
	}
	if IsDiscoveryCredential(apiKey, "codex") || IsDiscoveryCredential(apiKind, "claude") {
		t.Fatal("codex/claude API keys point at their own relays and must not qualify")
	}
	// xAI keys talk to xAI itself, so their listing describes the provider.
	if !IsDiscoveryCredential(xaiKey, "xai") {
		t.Fatal("xAI API key must qualify")
	}
	if IsDiscoveryCredential(nil, "codex") {
		t.Fatal("nil credential must not qualify")
	}
}

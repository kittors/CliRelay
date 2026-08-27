package authfiles

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type modelSourceStub struct {
	clientID string
	models   []*registry.ModelInfo
}

func (s *modelSourceStub) GetModelsForClient(clientID string) []*registry.ModelInfo {
	s.clientID = clientID
	return s.models
}

func TestModelLookupAuthIDUsesMatchingAuthID(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-id",
		FileName: "codex.json",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := ModelLookupAuthID(manager, "codex.json"); got != "auth-id" {
		t.Fatalf("ModelLookupAuthID() = %q, want auth-id", got)
	}
	if got := ModelLookupAuthID(manager, "missing.json"); got != "missing.json" {
		t.Fatalf("ModelLookupAuthID() fallback = %q, want missing.json", got)
	}
}

func TestListModelEntriesBuildsPublicPayload(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-id",
		FileName: "codex.json",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	source := &modelSourceStub{
		models: []*registry.ModelInfo{
			{
				ID:          "gpt-test",
				DisplayName: "GPT Test",
				Type:        "codex",
				OwnedBy:     "openai",
			},
			nil,
			{ID: "minimal"},
		},
	}

	got := ListModelEntries(manager, source, "codex.json")
	if source.clientID != "auth-id" {
		t.Fatalf("source clientID = %q, want auth-id", source.clientID)
	}
	if len(got) != 2 {
		t.Fatalf("entries length = %d, want 2: %#v", len(got), got)
	}
	if got[0]["id"] != "gpt-test" || got[0]["display_name"] != "GPT Test" || got[0]["type"] != "codex" || got[0]["owned_by"] != "openai" {
		t.Fatalf("entry[0] = %#v, want full public payload", got[0])
	}
	if _, ok := got[1]["display_name"]; ok {
		t.Fatalf("minimal entry has display_name: %#v", got[1])
	}
	if got[1]["id"] != "minimal" {
		t.Fatalf("minimal id = %#v, want minimal", got[1]["id"])
	}
}

type modelRegistrarStub struct {
	calls int
	last  struct {
		clientID string
		provider string
		models   []*registry.ModelInfo
	}
}

func (s *modelRegistrarStub) RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo) {
	s.calls++
	s.last.clientID = clientID
	s.last.provider = clientProvider
	s.last.models = models
}

func TestListModelEntriesLiveForTenantCodexDoesNotReplaceRegistry(t *testing.T) {
	ResetDiscoveryCacheForTest()
	// Without a real upstream, refresh falls back to registry; registrar must not be called
	// when live is empty. When we inject via a custom path, codex updateRegistry is false.
	// Here we only assert the empty-live path never registers.
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth",
		FileName: "codex.json",
		Provider: "codex",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	source := &modelSourceStub{
		models: []*registry.ModelInfo{{ID: "gpt-5.5", DisplayName: "GPT-5.5", Type: "codex"}},
	}
	reg := &modelRegistrarStub{}
	models, label := ListModelEntriesLiveForTenant(
		context.Background(),
		manager,
		source,
		reg,
		nil,
		"",
		"codex.json",
		true,
	)
	if label != "registry" {
		t.Fatalf("source label = %q, want registry (no live without credentials)", label)
	}
	if reg.calls != 0 {
		t.Fatalf("RegisterClient calls = %d, want 0 for failed/empty codex live", reg.calls)
	}
	if len(models) != 1 || models[0]["id"] != "gpt-5.5" {
		t.Fatalf("fallback models = %#v", models)
	}
}

func TestListModelEntriesLiveWithoutRefreshUsesRegistryWhenNoDiscoveryCache(t *testing.T) {
	ResetDiscoveryCacheForTest()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth",
		FileName: "codex.json",
		Provider: "codex",
		// no credentials → warm fails → registry fallback
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	source := &modelSourceStub{
		models: []*registry.ModelInfo{{ID: "gpt-static"}},
	}
	reg := &modelRegistrarStub{}
	models, label := ListModelEntriesLiveForTenant(
		context.Background(), manager, source, reg, nil, "", "codex.json", false,
	)
	if label != "registry" || reg.calls != 0 || len(models) != 1 || models[0]["id"] != "gpt-static" {
		t.Fatalf("label=%q calls=%d models=%#v", label, reg.calls, models)
	}
}

func TestDiscoveryCacheSharedAcrossSameProviderAccounts(t *testing.T) {
	ResetDiscoveryCacheForTest()
	storeDiscoveryCache("", "codex", []*registry.ModelInfo{
		{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", Type: "codex", OwnedBy: "openai"},
		{ID: "gpt-5.5", DisplayName: "GPT-5.5", Type: "codex", OwnedBy: "openai"},
	})

	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "codex-a", FileName: "codex-a.json", Provider: "codex"},
		{ID: "codex-b", FileName: "codex-b.json", Provider: "codex"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", auth.ID, err)
		}
	}
	source := &modelSourceStub{
		models: []*registry.ModelInfo{{ID: "gpt-static-only"}},
	}
	reg := &modelRegistrarStub{}

	// Open without refresh must serve discovery cache, not static registry.
	for _, name := range []string{"codex-a.json", "codex-b.json"} {
		models, label := ListModelEntriesLiveForTenant(
			context.Background(), manager, source, reg, nil, "", name, false,
		)
		if label != "upstream" {
			t.Fatalf("%s source=%q, want upstream", name, label)
		}
		if reg.calls != 0 {
			t.Fatalf("RegisterClient must not run for shared discovery, calls=%d", reg.calls)
		}
		// Discovery supplements the static catalog rather than replacing it, so the
		// assertion is that the discovered models are served — not that they are
		// the only ones. Replacing the catalog is what hid gpt-image-2 from the panel.
		if !containsModelID(models, "gpt-5.6-sol") || !containsModelID(models, "gpt-5.5") {
			t.Fatalf("%s did not serve the shared discovery result: %#v", name, models)
		}
	}

	// Claude cache must not leak into codex and vice versa.
	storeDiscoveryCache("", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4", Type: "claude"},
	})
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "claude-a", FileName: "claude-a.json", Provider: "claude",
	}); err != nil {
		t.Fatalf("Register claude: %v", err)
	}
	claudeModels, claudeLabel := ListModelEntriesLiveForTenant(
		context.Background(), manager, source, reg, nil, "", "claude-a.json", false,
	)
	// The point of this assertion is cache isolation: claude serves its own
	// discovery result and none of codex's.
	if claudeLabel != "upstream" || !containsModelID(claudeModels, "claude-sonnet-4") {
		t.Fatalf("claude models=%#v label=%q", claudeModels, claudeLabel)
	}
	if containsModelID(claudeModels, "gpt-5.6-sol") {
		t.Fatalf("codex discovery leaked into claude: %#v", claudeModels)
	}
	// codex still its own cache
	codexModels, codexLabel := ListModelEntriesLiveForTenant(
		context.Background(), manager, source, reg, nil, "", "codex-a.json", false,
	)
	if codexLabel != "upstream" || !containsModelID(codexModels, "gpt-5.6-sol") {
		t.Fatalf("codex still shared cache: %#v label=%q", codexModels, codexLabel)
	}
}

func TestDiscoveryCacheDoesNotReplaceRegistrar(t *testing.T) {
	ResetDiscoveryCacheForTest()
	storeDiscoveryCache("", "codex", []*registry.ModelInfo{{ID: "gpt-5.6-terra"}})
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-auth", FileName: "codex.json", Provider: "codex",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	source := &modelSourceStub{models: []*registry.ModelInfo{{ID: "static"}}}
	reg := &modelRegistrarStub{}
	_, _ = ListModelEntriesLiveForTenant(
		context.Background(), manager, source, reg, nil, "", "codex.json", true,
	)
	// force with no credentials fails warm; registrar still never called
	if reg.calls != 0 {
		t.Fatalf("RegisterClient calls=%d want 0", reg.calls)
	}
}

func TestXAISharedDiscoveryCacheAcrossAccounts(t *testing.T) {
	ResetDiscoveryCacheForTest()
	// Seed via grok alias; xai accounts must hit the same cache key.
	storeDiscoveryCache("", "grok", []*registry.ModelInfo{
		{ID: "grok-4", DisplayName: "Grok 4", Type: "xai", OwnedBy: "xai"},
		{ID: "grok-3-mini", DisplayName: "Grok 3 Mini", Type: "xai", OwnedBy: "xai"},
	})
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{ID: "xai-a", FileName: "xai-a.json", Provider: "xai"},
		{ID: "xai-b", FileName: "xai-b.json", Provider: "xai"},
		{ID: "grok-c", FileName: "grok-c.json", Provider: "grok"},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", auth.ID, err)
		}
	}
	source := &modelSourceStub{models: []*registry.ModelInfo{{ID: "static-xai"}}}
	reg := &modelRegistrarStub{}
	for _, name := range []string{"xai-a.json", "xai-b.json", "grok-c.json"} {
		models, label := ListModelEntriesLiveForTenant(
			context.Background(), manager, source, reg, nil, "", name, false,
		)
		if label != "upstream" {
			t.Fatalf("%s label=%q want upstream", name, label)
		}
		if !containsModelID(models, "grok-4") {
			t.Fatalf("%s did not serve the shared xai discovery result: %#v", name, models)
		}
	}
	if reg.calls != 0 {
		t.Fatalf("RegisterClient calls=%d want 0 for xai discovery path", reg.calls)
	}
}

func TestSupportsSharedDiscoveryIncludesXAI(t *testing.T) {
	for _, provider := range []string{"xai", "x-ai", "grok", "XAI", "Claude", "codex", "kimi", "Kimi"} {
		if !SupportsSharedDiscovery(provider) {
			t.Fatalf("SupportsSharedDiscovery(%q)=false, want true", provider)
		}
	}
	if SupportsSharedDiscovery("antigravity") {
		t.Fatalf("antigravity must not use shared discovery")
	}
}

// Kimi used to serve the compiled-in catalog here, so the panel showed the same
// four ids no matter what Moonshot had shipped. The discovery cache is what makes
// the panel reflect the gateway instead.
func TestKimiSharedDiscoveryServesCacheWithoutRegistrar(t *testing.T) {
	ResetDiscoveryCacheForTest()
	storeDiscoveryCache("", "kimi", []*registry.ModelInfo{
		{ID: "kimi-k2.7", DisplayName: "Kimi K2.7", Type: "kimi", OwnedBy: "moonshot"},
	})
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "kimi-a", FileName: "kimi-a.json", Provider: "kimi",
	}); err != nil {
		t.Fatalf("Register kimi: %v", err)
	}
	source := &modelSourceStub{models: []*registry.ModelInfo{{ID: "kimi-k2"}}}
	reg := &modelRegistrarStub{}

	models, label := ListModelEntriesLiveForTenant(
		context.Background(), manager, source, reg, nil, "", "kimi-a.json", false,
	)
	if label != "upstream" {
		t.Fatalf("label=%q want upstream", label)
	}
	if !containsModelID(models, "kimi-k2.7") {
		t.Fatalf("kimi discovery result not served: %#v", models)
	}
	if containsModelID(models, "kimi-k2") {
		t.Fatalf("static catalog leaked into the kimi discovery result: %#v", models)
	}
	if reg.calls != 0 {
		t.Fatalf("RegisterClient calls=%d want 0 for kimi discovery path", reg.calls)
	}
}

// containsModelID reports whether a rendered model list carries an id.
func containsModelID(models []map[string]any, id string) bool {
	for _, model := range models {
		if got, _ := model["id"].(string); got == id {
			return true
		}
	}
	return false
}

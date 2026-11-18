package modelcatalog

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The management catalog is built from the static registry, which never looked
// at model_configs.Enabled. Disabling a registry-backed model therefore left it
// listed in the management model picker; the disabled-model filter is the step that
// makes the toggle mean something there.

func seedEnablement(t *testing.T, modelID string, enabled bool) {
	t.Helper()
	if _, err := modelconfigsettings.UpsertConfig(modelconfigsettings.UpsertConfigInput{
		ModelID:     modelID,
		Scope:       "active",
		OwnedBy:     "anthropic",
		Enabled:     enabled,
		PricingMode: "call",
	}); err != nil {
		t.Fatalf("UpsertConfig(%q) error = %v", modelID, err)
	}
}

func modelIDsOf(models []map[string]any) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		out = append(out, id)
	}
	return out
}

func TestDropDisabledModelsRemovesDisabledEntries(t *testing.T) {
	initModelCatalogTestDB(t)

	seedEnablement(t, "claude-opus-4-6", false)
	seedEnablement(t, "claude-sonnet-4-6", false)
	seedEnablement(t, "claude-opus-5", true)

	models := []map[string]any{
		{"id": "claude-opus-4-6"},
		{"id": "claude-opus-5"},
		{"id": "claude-sonnet-4-6"},
		{"id": "claude-fable-5-1"},
	}

	got := modelIDsOf(modelconfigsettings.FilterOutDisabled("", models))

	want := []string{"claude-opus-5", "claude-fable-5-1"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func TestDropDisabledModelsKeepsEverythingWhenNothingDisabled(t *testing.T) {
	initModelCatalogTestDB(t)

	seedEnablement(t, "claude-opus-5", true)

	models := []map[string]any{{"id": "claude-opus-5"}, {"id": "claude-fable-5-1"}}
	if got := modelconfigsettings.FilterOutDisabled("", models); len(got) != 2 {
		t.Fatalf("FilterOutDisabled() = %v, want both models", modelIDsOf(got))
	}
}

func TestDropDisabledModelsMatchesCaseInsensitively(t *testing.T) {
	initModelCatalogTestDB(t)

	seedEnablement(t, "claude-opus-4-6", false)

	models := []map[string]any{{"id": "Claude-Opus-4-6"}, {"id": "claude-opus-5"}}
	got := modelIDsOf(modelconfigsettings.FilterOutDisabled("", models))
	if len(got) != 1 || got[0] != "claude-opus-5" {
		t.Fatalf("ids = %v, want [claude-opus-5]", got)
	}
}

func TestDropDisabledModelsHandlesEmptyInput(t *testing.T) {
	initModelCatalogTestDB(t)

	seedEnablement(t, "claude-opus-4-6", false)

	if got := modelconfigsettings.FilterOutDisabled("", nil); len(got) != 0 {
		t.Fatalf("FilterOutDisabled(nil) = %v, want empty", modelIDsOf(got))
	}
}

func TestDropDisabledModelIDsRemovesDisabledEntries(t *testing.T) {
	initModelCatalogTestDB(t)

	seedEnablement(t, "claude-opus-4-6", false)
	seedEnablement(t, "claude-opus-5", true)

	ids := map[string]struct{}{"claude-opus-4-6": {}, "claude-opus-5": {}, "claude-fable-5-1": {}}
	got := modelconfigsettings.FilterOutDisabledIDs("", ids)

	if _, present := got["claude-opus-4-6"]; present {
		t.Fatal("disabled model survived the ID filter")
	}
	for _, want := range []string{"claude-opus-5", "claude-fable-5-1"} {
		if _, present := got[want]; !present {
			t.Fatalf("%q was dropped, want it kept", want)
		}
	}
}

// The management catalog page intersects the model_configs rows with configured
// availability. Dropping a disabled model from availability therefore removed the
// row carrying its toggle: the model disappeared from the page entirely and could
// never be switched back on.
func TestConfiguredAvailabilityKeepsDisabledModelsForTheOperator(t *testing.T) {
	initModelCatalogTestDB(t)

	const (
		clientID     = "enablement-visibility-auth"
		disabledID   = "enablement-visibility-disabled"
		enabledModel = "enablement-visibility-enabled"
	)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(clientID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })
	modelRegistry.RegisterClient(clientID, "codex", []*registry.ModelInfo{
		{ID: disabledID, Object: "model", OwnedBy: "openai"},
		{ID: enabledModel, Object: "model", OwnedBy: "openai"},
	})

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: clientID, Provider: "codex", Label: "Codex", Status: coreauth.StatusActive,
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	seedEnablement(t, disabledID, false)
	seedEnablement(t, enabledModel, true)

	service := New(&config.Config{}, manager)
	data, ok := service.ConfiguredAvailability("", "")["data"].([]map[string]any)
	if !ok {
		t.Fatal("configured availability returned no data slice")
	}

	var found map[string]any
	for _, item := range data {
		if item["id"] == disabledID {
			found = item
			break
		}
	}
	if found == nil {
		t.Fatalf("disabled model %q is missing from the operator catalog; ids=%v", disabledID, modelIDsOf(data))
	}
	if found["enabled"] != false {
		t.Fatalf("disabled model entry enabled = %#v, want false", found["enabled"])
	}

	// ... while the surfaces that serve models still hide it.
	visible := service.PortalVisibleModelIDs("", "")
	if _, present := visible[disabledID]; present {
		t.Fatal("disabled model is still portal-visible")
	}
	if _, present := visible[enabledModel]; !present {
		t.Fatalf("enabled model %q lost its portal visibility", enabledModel)
	}
}

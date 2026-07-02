package usage

import "testing"

func TestBuildCcSwitchCodexModelCatalogUsesRequestModels(t *testing.T) {
	row := CcSwitchImportConfigRow{
		ClientType:   "codex",
		DefaultModel: "gpt-5.5",
		ModelMappings: []CcSwitchModelMappingRow{
			{RequestModel: "gpt-5.5", TargetModel: "gpt-5.5"},
			{RequestModel: "deepseek-v4-flash", TargetModel: "deepseek-chat"},
			{RequestModel: "DeepSeek-V4-Flash", TargetModel: "deepseek-chat"},
			{RequestModel: "deepseek-v4-pro", TargetModel: "deepseek-reasoner"},
		},
	}

	catalog := BuildCcSwitchCodexModelCatalog(row)
	if catalog == nil {
		t.Fatal("catalog = nil, want generated catalog")
	}

	got := make([]string, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		got = append(got, model.Slug)
	}
	want := []string{"gpt-5.5", "deepseek-v4-flash", "deepseek-v4-pro"}
	if len(got) != len(want) {
		t.Fatalf("slugs len = %d, want %d: %#v", len(got), len(want), got)
	}
	for idx := range want {
		if got[idx] != want[idx] {
			t.Fatalf("slug[%d] = %q, want %q; all=%#v", idx, got[idx], want[idx], got)
		}
	}

	deepseek := catalog.Models[1]
	if deepseek.Model != deepseek.Slug {
		t.Fatalf("model = %q, want same as slug %q", deepseek.Model, deepseek.Slug)
	}
	if deepseek.ContextWindow != ccSwitchCodexDefaultContextWindow {
		t.Fatalf("context_window = %d, want %d", deepseek.ContextWindow, ccSwitchCodexDefaultContextWindow)
	}
	if deepseek.ModelMessages.ContextWindow != ccSwitchCodexDefaultContextWindow {
		t.Fatalf("model_messages.context_window = %d, want %d", deepseek.ModelMessages.ContextWindow, ccSwitchCodexDefaultContextWindow)
	}
	if !deepseek.SupportedInAPI || deepseek.Visibility != "list" {
		t.Fatalf("catalog entry missing visible API flags: %#v", deepseek)
	}
	if deepseek.BaseInstructions == "" || deepseek.ModelMessages.InstructionsTemplate == "" {
		t.Fatalf("catalog entry missing Codex instruction templates: %#v", deepseek)
	}
}

func TestBuildCcSwitchCodexModelCatalogSkipsNonCodex(t *testing.T) {
	row := CcSwitchImportConfigRow{
		ClientType:   "claude",
		DefaultModel: "claude-sonnet-4-5",
	}

	if catalog := BuildCcSwitchCodexModelCatalog(row); catalog != nil {
		t.Fatalf("catalog = %#v, want nil", catalog)
	}
}

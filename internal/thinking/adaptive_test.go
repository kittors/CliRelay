package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestConvertMaxLevelRoundTrips(t *testing.T) {
	if budget, ok := ConvertLevelToBudget("max"); !ok || budget != BudgetMax {
		t.Fatalf(`ConvertLevelToBudget("max") = %d, %v; want %d, true`, budget, ok, BudgetMax)
	}
	for _, level := range []ThinkingLevel{LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax} {
		budget, ok := ConvertLevelToBudget(string(level))
		if !ok {
			t.Fatalf("ConvertLevelToBudget(%q) failed", level)
		}
		if got, _ := ConvertBudgetToLevel(budget); got != string(level) {
			t.Errorf("ConvertBudgetToLevel(ConvertLevelToBudget(%q)=%d) = %q", level, budget, got)
		}
	}
}

// Budgets clients really send stay where they were: only BudgetMax and above
// read as "max", and no Claude request can carry that (budget < max_tokens <= 128000).
func TestConvertBudgetToLevelBoundaries(t *testing.T) {
	cases := map[int]string{
		24576:  "high",
		24577:  "xhigh",
		31999:  "xhigh",
		63999:  "xhigh",
		127999: "xhigh",
		128000: "max",
		200000: "max",
	}
	for budget, want := range cases {
		if got, _ := ConvertBudgetToLevel(budget); got != want {
			t.Errorf("ConvertBudgetToLevel(%d) = %q, want %q", budget, got, want)
		}
	}
}

func TestLevelAbove(t *testing.T) {
	cases := []struct {
		level, ceiling ThinkingLevel
		want           bool
	}{
		{LevelMax, LevelHigh, true},
		{LevelXHigh, LevelHigh, true},
		{LevelHigh, LevelHigh, false},
		{LevelLow, LevelHigh, false},
		{"", LevelHigh, false},
		{LevelNone, LevelLow, false},
		{"ultra", LevelHigh, false},
		{LevelMax, "", false},
	}
	for _, tc := range cases {
		if got := LevelAbove(tc.level, tc.ceiling); got != tc.want {
			t.Errorf("LevelAbove(%q, %q) = %v, want %v", tc.level, tc.ceiling, got, tc.want)
		}
	}
}

// A model without "max" clamps it to its highest level instead of failing.
func TestClampLevelMaxToHighestSupported(t *testing.T) {
	info := &registry.ModelInfo{ID: "no-max", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}}
	if got := clampLevel(LevelMax, info, "codex"); got != LevelXHigh {
		t.Fatalf("clampLevel(max) = %q, want xhigh", got)
	}
}

func TestExtractClaudeConfigReadsAdaptiveShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want ThinkingConfig
	}{
		{name: "adaptive with effort", body: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`, want: ThinkingConfig{Mode: ModeLevel, Level: LevelXHigh}},
		{name: "adaptive without effort", body: `{"thinking":{"type":"adaptive","display":"summarized"}}`, want: ThinkingConfig{Mode: ModeAuto, Budget: -1}},
		{name: "between_tools", body: `{"thinking":{"type":"between_tools"},"output_config":{"effort":"high"}}`, want: ThinkingConfig{Mode: ModeNone}},
		// Opus 4.7 runs effort with thinking off; that is not a thinking request.
		{name: "effort alone", body: `{"output_config":{"effort":"high"}}`, want: ThinkingConfig{}},
		{name: "disabled wins over effort", body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, want: ThinkingConfig{Mode: ModeNone}},
		{name: "budget", body: `{"thinking":{"type":"enabled","budget_tokens":4096}}`, want: ThinkingConfig{Mode: ModeBudget, Budget: 4096}},
		{name: "enabled without budget", body: `{"thinking":{"type":"enabled"}}`, want: ThinkingConfig{Mode: ModeAuto, Budget: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractClaudeConfig([]byte(tc.body)); got != tc.want {
				t.Fatalf("extractClaudeConfig = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A request already in the adaptive shape is forwarded as is on every model.
// between_tools is read as "off" only for adaptive models, and on them an
// explicit effort beats a budget sent next to it.
func TestClaudeShapesAreReadPerModel(t *testing.T) {
	adaptive := registry.LookupStaticModelInfo("claude-opus-5-5")
	budget := registry.LookupStaticModelInfo("claude-opus-4-6")
	if adaptive == nil || budget == nil {
		t.Fatal("static Claude models missing")
	}
	cases := []struct {
		name string
		body string
		info *registry.ModelInfo
		want ThinkingConfig
	}{
		{name: "adaptive on adaptive model", body: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, info: adaptive},
		{name: "adaptive with unlisted effort", body: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"}}`, info: adaptive},
		{name: "adaptive on budget model", body: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, info: budget},
		{name: "adaptive on unknown model", body: `{"thinking":{"type":"adaptive"}}`},
		{name: "between_tools on adaptive model", body: `{"thinking":{"type":"between_tools"}}`, info: adaptive, want: ThinkingConfig{Mode: ModeNone}},
		{name: "between_tools on budget model", body: `{"thinking":{"type":"between_tools"}}`, info: budget},
		{name: "between_tools on unknown model", body: `{"thinking":{"type":"between_tools"}}`},
		{name: "effort beats budget on adaptive model", body: `{"thinking":{"type":"enabled","budget_tokens":1024},"output_config":{"effort":"max"}}`, info: adaptive, want: ThinkingConfig{Mode: ModeLevel, Level: LevelMax}},
		{name: "effort beats enabled on adaptive model", body: `{"thinking":{"type":"enabled"},"output_config":{"effort":"low"}}`, info: adaptive, want: ThinkingConfig{Mode: ModeLevel, Level: LevelLow}},
		{name: "budget alone on adaptive model", body: `{"thinking":{"type":"enabled","budget_tokens":2048}}`, info: adaptive, want: ThinkingConfig{Mode: ModeBudget, Budget: 2048}},
		{name: "disabled keeps its meaning next to effort", body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, info: adaptive, want: ThinkingConfig{Mode: ModeNone}},
		// Budget models take budget and effort side by side.
		{name: "budget with effort on budget model", body: `{"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"max"}}`, info: budget, want: ThinkingConfig{Mode: ModeBudget, Budget: 2048}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractThinkingConfigForModel([]byte(tc.body), "claude", tc.info); got != tc.want {
				t.Fatalf("extractThinkingConfigForModel = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestNormalizeConfiguredAdaptiveModelTurnsBudgetsIntoLevels(t *testing.T) {
	static := registry.LookupStaticModelInfo("claude-sonnet-5-5")
	if static == nil {
		t.Fatal("claude-sonnet-5-5 missing")
	}
	configured := &registry.ModelInfo{ID: "team-sonnet", UserDefined: true, Thinking: static.Thinking}

	cases := []struct {
		in   ThinkingConfig
		want ThinkingConfig
	}{
		{in: ThinkingConfig{Mode: ModeBudget, Budget: 24576}, want: ThinkingConfig{Mode: ModeLevel, Level: LevelHigh}},
		{in: ThinkingConfig{Mode: ModeBudget, Budget: BudgetMax}, want: ThinkingConfig{Mode: ModeLevel, Level: LevelMax}},
		// minimal is below everything Claude lists.
		{in: ThinkingConfig{Mode: ModeBudget, Budget: 512}, want: ThinkingConfig{Mode: ModeLevel, Level: LevelLow}},
		{in: ThinkingConfig{Mode: ModeLevel, Level: LevelXHigh}, want: ThinkingConfig{Mode: ModeLevel, Level: LevelXHigh}},
		// A suffix such as (minimal) lands on the model's lowest level too.
		{in: ThinkingConfig{Mode: ModeLevel, Level: LevelMinimal}, want: ThinkingConfig{Mode: ModeLevel, Level: LevelLow}},
		{in: ThinkingConfig{Mode: ModeNone}, want: ThinkingConfig{Mode: ModeNone}},
		{in: ThinkingConfig{Mode: ModeAuto, Budget: -1}, want: ThinkingConfig{Mode: ModeAuto, Budget: -1}},
	}
	for _, tc := range cases {
		if got := normalizeUserDefinedConfig(tc.in, configured, "openai", "claude"); got != tc.want {
			t.Errorf("normalizeUserDefinedConfig(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}

	// Without the adaptive rules the old conversion still applies.
	level := ThinkingConfig{Mode: ModeLevel, Level: LevelHigh}
	if got := normalizeUserDefinedConfig(level, &registry.ModelInfo{ID: "plain", UserDefined: true}, "openai", "claude"); got != (ThinkingConfig{Mode: ModeBudget, Budget: 24576}) {
		t.Errorf("non-adaptive configured model: %+v, want budget 24576", got)
	}
}

// "max" on a configured budget model is that model's own ceiling when its
// upstream name resolved to a catalog entry, and BudgetMax otherwise.
func TestNormalizeConfiguredBudgetModelCapsMaxAtTheModelCeiling(t *testing.T) {
	maxLevel := ThinkingConfig{Mode: ModeLevel, Level: LevelMax}
	gemini := &registry.ModelInfo{ID: "team-gemini", UserDefined: true, Thinking: &registry.ThinkingSupport{Min: 128, Max: 32768}}
	if got := normalizeUserDefinedConfig(maxLevel, gemini, "openai", "gemini"); got != (ThinkingConfig{Mode: ModeBudget, Budget: 32768}) {
		t.Errorf("resolved Gemini model: %+v, want budget 32768", got)
	}
	if got := normalizeUserDefinedConfig(maxLevel, &registry.ModelInfo{ID: "team-unknown", UserDefined: true}, "openai", "gemini"); got != (ThinkingConfig{Mode: ModeBudget, Budget: BudgetMax}) {
		t.Errorf("unresolved model: %+v, want budget %d", got, BudgetMax)
	}
	// Levels that fit stay as they were.
	if got := normalizeUserDefinedConfig(ThinkingConfig{Mode: ModeLevel, Level: LevelHigh}, gemini, "openai", "gemini"); got != (ThinkingConfig{Mode: ModeBudget, Budget: 24576}) {
		t.Errorf("high on resolved Gemini model: %+v, want budget 24576", got)
	}
}

func TestExtractThinkingLevelReportsAdaptiveEffort(t *testing.T) {
	if got := ExtractThinkingLevel([]byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`), "claude"); got != "xhigh" {
		t.Fatalf("ExtractThinkingLevel(adaptive xhigh) = %q", got)
	}
	if got := ExtractThinkingLevel([]byte(`{"thinking":{"type":"between_tools"}}`), "claude"); got != "none" {
		t.Fatalf("ExtractThinkingLevel(between_tools) = %q, want none", got)
	}
}

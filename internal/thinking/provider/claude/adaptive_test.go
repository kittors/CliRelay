package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

func adaptiveModel(off, offMaxLevel string) *registry.ModelInfo {
	return &registry.ModelInfo{
		ID: "adaptive-test-model",
		Thinking: &registry.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         []string{"low", "medium", "high", "xhigh", "max"},
			Adaptive:       &registry.AdaptiveThinking{Off: off, OffMaxLevel: offMaxLevel},
		},
	}
}

func applyTo(t *testing.T, body string, config thinking.ThinkingConfig, info *registry.ModelInfo) []byte {
	t.Helper()
	out, err := NewApplier().Apply([]byte(body), config, info)
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	return out
}

func TestAdaptiveLevelReplacesTheBudgetForm(t *testing.T) {
	info := adaptiveModel(registry.ClaudeThinkingOffDisabled, "")
	for _, body := range []string{
		`{"max_tokens":32000,"thinking":{"type":"enabled","budget_tokens":24576}}`,
		`{"max_tokens":32000,"thinking":{"type":"between_tools"}}`,
		`{"max_tokens":32000}`,
	} {
		out := applyTo(t, body, thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh}, info)
		if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"adaptive","display":"summarized"}` {
			t.Errorf("%s: thinking = %s", body, got)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
			t.Errorf("%s: effort = %q", body, got)
		}
		if got := gjson.GetBytes(out, "max_tokens").Int(); got != 32000 {
			t.Errorf("%s: max_tokens = %d, want it untouched", body, got)
		}
	}
}

// A request that already chose adaptive thinking keeps its own fields.
func TestAdaptiveLevelKeepsTheRequestsAdaptiveFields(t *testing.T) {
	info := adaptiveModel(registry.ClaudeThinkingAlwaysOn, "")
	body := `{"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"low","format":{"type":"json_schema"}}}`
	out := applyTo(t, body, thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelMax}, info)

	if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"adaptive","display":"omitted"}` {
		t.Fatalf("thinking = %s", got)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "max" {
		t.Fatalf("effort = %q, want max", got)
	}
	if got := gjson.GetBytes(out, "output_config.format.type").String(); got != "json_schema" {
		t.Fatalf("output_config.format lost: %s", out)
	}
}

func TestAdaptiveAutoKeepsTheRequestsEffort(t *testing.T) {
	info := adaptiveModel(registry.ClaudeThinkingOffDisabled, "")
	out := applyTo(t, `{"thinking":{"type":"enabled"},"output_config":{"effort":"medium"}}`, thinking.ThinkingConfig{Mode: thinking.ModeAuto, Budget: -1}, info)
	if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"adaptive","display":"summarized"}` {
		t.Fatalf("thinking = %s", got)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "medium" {
		t.Fatalf("effort = %q, want medium", got)
	}
}

func TestAdaptiveOffFollowsTheModelsRule(t *testing.T) {
	cases := []struct {
		name         string
		off          string
		offMaxLevel  string
		body         string
		wantThinking string
		wantEffort   string
	}{
		{name: "always on, no effort", off: registry.ClaudeThinkingAlwaysOn, body: `{"thinking":{"type":"disabled"}}`, wantEffort: "low"},
		{name: "always on keeps effort", off: registry.ClaudeThinkingAlwaysOn, body: `{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, wantEffort: "high"},
		{name: "disabled", off: registry.ClaudeThinkingOffDisabled, body: `{"thinking":{"type":"adaptive","display":"summarized"}}`, wantThinking: `{"type":"disabled"}`},
		{name: "disabled under the cap", off: registry.ClaudeThinkingOffDisabled, offMaxLevel: "high", body: `{"output_config":{"effort":"high"}}`, wantThinking: `{"type":"disabled"}`, wantEffort: "high"},
		{name: "disabled over the cap", off: registry.ClaudeThinkingOffDisabled, offMaxLevel: "high", body: `{"output_config":{"effort":"xhigh"}}`, wantThinking: `{"type":"disabled"}`, wantEffort: "high"},
		{name: "between_tools takes no other field", off: registry.ClaudeThinkingOffBetweenTools, offMaxLevel: "high", body: `{"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max"}}`, wantThinking: `{"type":"between_tools"}`, wantEffort: "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := applyTo(t, tc.body, thinking.ThinkingConfig{Mode: thinking.ModeNone}, adaptiveModel(tc.off, tc.offMaxLevel))
			if got := gjson.GetBytes(out, "thinking").Raw; got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %q", got, tc.wantThinking)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

// ValidateConfig turns budgets into levels before Apply; a direct caller that
// hands over a budget still gets no budget_tokens.
func TestAdaptiveBudgetIsConvertedEvenWithoutValidation(t *testing.T) {
	info := adaptiveModel(registry.ClaudeThinkingAlwaysOn, "")
	cases := map[int]string{512: "low", 8192: "medium", thinking.BudgetMax: "max"}
	for budget, want := range cases {
		out := applyTo(t, `{}`, thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: budget}, info)
		if gjson.GetBytes(out, "thinking.budget_tokens").Exists() {
			t.Fatalf("budget %d: budget_tokens written: %s", budget, out)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != want {
			t.Errorf("budget %d: effort = %q, want %q", budget, got, want)
		}
	}
	out := applyTo(t, `{"thinking":{"type":"enabled","budget_tokens":0}}`, thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 0}, info)
	if gjson.GetBytes(out, "thinking").Exists() || gjson.GetBytes(out, "output_config.effort").String() != "low" {
		t.Fatalf("budget 0 on an always-on model: %s", out)
	}
}

// Configured models whose upstream name is adaptive-only get the same shape.
func TestConfiguredAdaptiveModelUsesAdaptiveThinking(t *testing.T) {
	info := adaptiveModel(registry.ClaudeThinkingOffBetweenTools, "high")
	info.UserDefined = true
	out := applyTo(t, `{"max_tokens":32000}`, thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelXHigh}, info)
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive: %s", got, out)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "xhigh" {
		t.Fatalf("effort = %q", got)
	}
}

// budget_tokens >= max_tokens is a 400 on every model, configured ones included.
func TestCompatibleBudgetStaysBelowMaxTokens(t *testing.T) {
	configured := &registry.ModelInfo{ID: "team-claude", UserDefined: true}
	cases := []struct {
		name      string
		body      string
		budget    int
		wantFinal int64
	}{
		{name: "max under default max_tokens", body: `{"max_tokens":32000}`, budget: thinking.BudgetMax, wantFinal: 31999},
		{name: "xhigh under default max_tokens", body: `{"max_tokens":32000}`, budget: 32768, wantFinal: 31999},
		{name: "fits", body: `{"max_tokens":64000}`, budget: 24576, wantFinal: 24576},
		{name: "no max_tokens to compare with", body: `{}`, budget: 24576, wantFinal: 24576},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, info := range []*registry.ModelInfo{configured, nil} {
				out := applyTo(t, tc.body, thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: tc.budget}, info)
				if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != tc.wantFinal {
					t.Fatalf("budget_tokens = %d, want %d (%s)", got, tc.wantFinal, out)
				}
				if got := gjson.GetBytes(out, "thinking.type").String(); got != "enabled" {
					t.Fatalf("thinking.type = %q", got)
				}
			}
		})
	}
}

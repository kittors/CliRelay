package test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func registerClaudeEffortFixture(t *testing.T, name string) {
	t.Helper()
	reg := sdkmodelcatalog.GlobalRegistry()
	reg.RegisterClient(name, "claude", sdkmodelcatalog.StaticModelDefinitionsByChannel("claude"))
	t.Cleanup(func() { reg.UnregisterClient(name) })
}

// prepareClaudeRequest mirrors the Claude executor: translate, carry native effort, apply thinking.
func prepareClaudeRequest(t *testing.T, format, model string, input []byte) []byte {
	t.Helper()
	body := sdktranslator.TranslateRequest(sdktranslator.FromString(format), sdktranslator.FromString("claude"), thinking.ParseSuffix(model).ModelName, input, true)
	body = thinking.ApplyClaudeNativeEffort(body, input, model, format)
	got, err := thinking.ApplyThinking(body, model, format, "claude", "claude")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func effortInput(effort string) []byte {
	return []byte(`{"input":"OK","messages":[{"role":"user","content":"OK"}],"reasoning":{"effort":"` + effort + `"},"reasoning_effort":"` + effort + `","max_output_tokens":16384}`)
}

func TestClaudeNativeEffortLevelsAndSuffixPriority(t *testing.T) {
	registerClaudeEffortFixture(t, "native-effort-fixture")
	for _, format := range []string{"openai-response", "openai"} {
		for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
			t.Run(format+"/"+effort, func(t *testing.T) {
				for _, tc := range []struct{ model, want string }{{"claude-opus-5-5", effort}, {"claude-opus-5-5(high)", "high"}} {
					got := prepareClaudeRequest(t, format, tc.model, effortInput(effort))
					if gjson.GetBytes(got, "thinking.type").String() != "adaptive" || gjson.GetBytes(got, "output_config.effort").String() != tc.want || gjson.GetBytes(got, "thinking.budget_tokens").Exists() {
						t.Fatalf("%s lost requested native effort: %s", tc.model, got)
					}
				}
			})
		}
	}
}

func TestClaudeNativeEffortSpecialValues(t *testing.T) {
	registerClaudeEffortFixture(t, "special-effort-fixture")
	for _, format := range []string{"openai-response", "openai"} {
		for _, tc := range []struct{ effort, thinkingType, nativeEffort string }{
			{"minimal", "adaptive", "low"},
			{"auto", "adaptive", ""},
			{"none", "disabled", ""},
			{"MAX", "adaptive", "max"},
		} {
			got := prepareClaudeRequest(t, format, "claude-opus-5-5", effortInput(tc.effort))
			if gjson.GetBytes(got, "thinking.type").String() != tc.thinkingType || gjson.GetBytes(got, "output_config.effort").String() != tc.nativeEffort || gjson.GetBytes(got, "thinking.budget_tokens").Exists() {
				t.Fatalf("%s/%s: unexpected thinking: %s", format, tc.effort, got)
			}
		}
	}
}

func TestClaudeExplicitBudgetSuffixOverridesNativeEffort(t *testing.T) {
	registerClaudeEffortFixture(t, "budget-suffix-fixture")
	for _, format := range []string{"openai-response", "openai"} {
		got := prepareClaudeRequest(t, format, "claude-opus-5-5(8192)", effortInput("high"))
		if gjson.GetBytes(got, "thinking.type").String() != "enabled" || gjson.GetBytes(got, "thinking.budget_tokens").Int() != 8192 {
			t.Fatalf("%s: explicit manual budget was changed: %s", format, got)
		}
		if gjson.GetBytes(got, "output_config.effort").Exists() {
			t.Fatalf("%s: overridden effort leaked alongside explicit budget: %s", format, got)
		}
	}
}

func TestClaudeLegacyModelKeepsBudgetTranslation(t *testing.T) {
	registerClaudeEffortFixture(t, "legacy-effort-fixture")
	for _, format := range []string{"openai-response", "openai"} {
		got := prepareClaudeRequest(t, format, "claude-sonnet-4-5-20250929", effortInput("medium"))
		if gjson.GetBytes(got, "thinking.type").String() != "enabled" || gjson.GetBytes(got, "thinking.budget_tokens").Int() != 8192 || gjson.GetBytes(got, "output_config.effort").Exists() {
			t.Fatalf("%s: legacy translation changed: %s", format, got)
		}
		for _, effort := range []string{"xhigh", "max"} {
			got := prepareClaudeRequest(t, format, "claude-sonnet-4-5-20250929", effortInput(effort))
			if gjson.GetBytes(got, "thinking.type").String() == "adaptive" || gjson.GetBytes(got, "output_config.effort").Exists() {
				t.Fatalf("%s/%s: legacy model received native effort: %s", format, effort, got)
			}
		}
	}
}

// Requests already in Claude's own format must pass through untouched.
func TestClaudeNativeRequestPassthrough(t *testing.T) {
	registerClaudeEffortFixture(t, "native-passthrough-fixture")
	bodies := []string{
		`{"output_config":{"effort":"high"},"max_tokens":32000}`,
		`{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"max_tokens":32000}`,
		`{"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},"max_tokens":32000}`,
		`{"thinking":{"type":"enabled","budget_tokens":4096},"output_config":{"effort":"low"},"max_tokens":32000}`,
		`{"thinking":{"type":"enabled","budget_tokens":4096},"max_tokens":16384}`,
	}
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-opus-4-7", "claude-opus-4-6", "claude-sonnet-4-5-20250929", "custom-claude-fixture"} {
		for _, input := range bodies {
			body := thinking.ApplyClaudeNativeEffort([]byte(input), []byte(input), model, "claude")
			got, err := thinking.ApplyThinking(body, model, "claude", "claude", "claude")
			if err != nil {
				t.Fatalf("%s %s: %v", model, input, err)
			}
			if string(got) != input {
				t.Fatalf("%s native request changed:\n got %s\nwant %s", model, got, input)
			}
		}
	}
}

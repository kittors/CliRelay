package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude Opus 4.7 and later answer thinking.type "enabled" with budget_tokens
// with a 400, and Opus 5.5, Fable and Sonnet 5.5 also reject "disabled". Every
// client format below carries its reasoning level as something the translators
// turn into a budget, so these cases run the same steps as ClaudeExecutor
// (translate, write back the model, ApplyThinking) and check what would go out.

const adaptiveSummarized = `{"type":"adaptive","display":"summarized"}`

// applyClaudeThinking mirrors ClaudeExecutor.Execute up to ApplyThinking.
func applyClaudeThinking(t *testing.T, from, model, payload string) []byte {
	t.Helper()
	src := sdktranslator.FromString(from)
	to := sdktranslator.FromString("claude")
	base := thinking.ParseSuffix(model).ModelName
	body := sdktranslator.TranslateRequest(src, to, base, []byte(payload), src != to)
	body, _ = sjson.SetBytes(body, "model", base)
	out, err := thinking.ApplyThinking(body, model, src.String(), to.String(), "claude")
	if err != nil {
		t.Fatalf("ApplyThinking(%s, %s) error: %v", from, model, err)
	}
	return out
}

// reasoningPayload is a request at the given reasoning level in each client
// format: Chat Completions, Responses, and Gemini's thinkingLevel.
func reasoningPayload(entry, model, level string) (from, payload string) {
	switch entry {
	case "chat":
		return "openai", fmt.Sprintf(`{"model":%q,"reasoning_effort":%q,"messages":[{"role":"user","content":"hi"}]}`, model, level)
	case "responses":
		return "openai-response", fmt.Sprintf(`{"model":%q,"reasoning":{"effort":%q},"input":"hi"}`, model, level)
	case "gemini":
		return "gemini", fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":%q}}}`, level)
	}
	panic("unknown entry " + entry)
}

var reasoningEntries = []string{"chat", "responses", "gemini"}

func TestClaudeAdaptiveModelsGetEffortForEveryLevel(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1", "claude-sonnet-5-5"} {
		for _, entry := range reasoningEntries {
			for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
				t.Run(model+"/"+entry+"/"+level, func(t *testing.T) {
					from, payload := reasoningPayload(entry, model, level)
					out := applyClaudeThinking(t, from, model, payload)

					if got := gjson.GetBytes(out, "thinking").Raw; got != adaptiveSummarized {
						t.Fatalf("thinking = %s, want %s", got, adaptiveSummarized)
					}
					if got := gjson.GetBytes(out, "output_config.effort").String(); got != level {
						t.Fatalf("output_config.effort = %q, want %q", got, level)
					}
				})
			}
		}
	}
}

// "none" maps onto each model's own way of turning thinking off.
func TestClaudeAdaptiveModelsTurnThinkingOffTheirOwnWay(t *testing.T) {
	cases := []struct {
		model        string
		wantThinking string // "" means the thinking field must be absent
		wantEffort   string
	}{
		// Thinking cannot be turned off: omit it and use the lowest effort.
		{model: "claude-opus-5-5", wantEffort: "low"},
		{model: "claude-fable-5-1", wantEffort: "low"},
		// Sonnet 5.5 rejects "disabled" and names between_tools instead.
		{model: "claude-sonnet-5-5", wantThinking: `{"type":"between_tools"}`},
		// Opus 5, Opus 4.7 and Sonnet 5 still accept "disabled".
		{model: "claude-opus-5", wantThinking: `{"type":"disabled"}`},
		{model: "claude-opus-4-7", wantThinking: `{"type":"disabled"}`},
		{model: "claude-sonnet-5", wantThinking: `{"type":"disabled"}`},
	}
	for _, tc := range cases {
		for _, entry := range reasoningEntries {
			t.Run(tc.model+"/"+entry, func(t *testing.T) {
				from, payload := reasoningPayload(entry, tc.model, "none")
				out := applyClaudeThinking(t, from, tc.model, payload)

				if got := gjson.GetBytes(out, "thinking").Raw; got != tc.wantThinking {
					t.Fatalf("thinking = %s, want %q", got, tc.wantThinking)
				}
				if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
					t.Fatalf("output_config.effort = %q, want %q", got, tc.wantEffort)
				}
			})
		}
	}
}

// Opus 4.6 still takes budgets and is left on them; "max" now reaches it as the
// largest budget max_tokens allows instead of being dropped.
func TestClaudeOpus46KeepsBudgetThinking(t *testing.T) {
	want := map[string]string{
		"low":    `{"type":"enabled","budget_tokens":1024}`,
		"medium": `{"type":"enabled","budget_tokens":8192}`,
		"high":   `{"type":"enabled","budget_tokens":24576}`,
		// The translators default max_tokens to 32000 and budget_tokens has
		// to stay below it.
		"xhigh": `{"type":"enabled","budget_tokens":31999}`,
		"max":   `{"type":"enabled","budget_tokens":31999}`,
		"none":  `{"type":"disabled"}`,
	}
	for _, entry := range reasoningEntries {
		for level, wantThinking := range want {
			t.Run(entry+"/"+level, func(t *testing.T) {
				from, payload := reasoningPayload(entry, "claude-opus-4-6", level)
				out := applyClaudeThinking(t, from, "claude-opus-4-6", payload)

				if got := gjson.GetBytes(out, "thinking").Raw; got != wantThinking {
					t.Fatalf("thinking = %s, want %s", got, wantThinking)
				}
				if got := gjson.GetBytes(out, "output_config").Raw; got != "" {
					t.Fatalf("output_config = %s, want none for a budget model", got)
				}
			})
		}
	}
}

// Gemini clients often send a raw budget rather than a level.
func TestClaudeAdaptiveModelsConvertGeminiBudgets(t *testing.T) {
	cases := []struct {
		budget     int
		wantEffort string
	}{
		{budget: 1024, wantEffort: "low"},
		{budget: 8192, wantEffort: "medium"},
		{budget: 24576, wantEffort: "high"},
		{budget: 32768, wantEffort: "xhigh"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.budget), func(t *testing.T) {
			payload := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":%d}}}`, tc.budget)
			out := applyClaudeThinking(t, "gemini", "claude-opus-5-5", payload)

			if got := gjson.GetBytes(out, "thinking").Raw; got != adaptiveSummarized {
				t.Fatalf("thinking = %s, want %s", got, adaptiveSummarized)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("output_config.effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}

	t.Run("zero turns thinking off", func(t *testing.T) {
		payload := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":0}}}`
		out := applyClaudeThinking(t, "gemini", "claude-sonnet-5-5", payload)
		if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"between_tools"}` {
			t.Fatalf("thinking = %s, want between_tools", got)
		}
	})
}

// "auto" asks the model to decide, which is what adaptive thinking does with no
// effort set.
func TestClaudeAdaptiveModelsAutoLeavesEffortToTheModel(t *testing.T) {
	from, payload := reasoningPayload("chat", "claude-opus-5-5", "auto")
	out := applyClaudeThinking(t, from, "claude-opus-5-5", payload)

	if got := gjson.GetBytes(out, "thinking").Raw; got != adaptiveSummarized {
		t.Fatalf("thinking = %s, want %s", got, adaptiveSummarized)
	}
	if got := gjson.GetBytes(out, "output_config.effort"); got.Exists() {
		t.Fatalf("output_config.effort = %s, want unset", got.Raw)
	}
}

func TestClaudeAdaptiveModelsHonorModelSuffix(t *testing.T) {
	cases := []struct {
		model        string
		wantThinking string
		wantEffort   string
	}{
		{model: "claude-opus-5-5(high)", wantThinking: adaptiveSummarized, wantEffort: "high"},
		{model: "claude-opus-5-5(max)", wantThinking: adaptiveSummarized, wantEffort: "max"},
		{model: "claude-fable-5-1(low)", wantThinking: adaptiveSummarized, wantEffort: "low"},
		{model: "claude-sonnet-5-5(none)", wantThinking: `{"type":"between_tools"}`},
		{model: "claude-opus-5-5(none)", wantEffort: "low"},
		// A numeric suffix is a budget and becomes the nearest effort.
		{model: "claude-sonnet-5-5(20000)", wantThinking: adaptiveSummarized, wantEffort: "high"},
		// Control: Opus 4.6 turns a level suffix into a budget below max_tokens.
		{model: "claude-opus-4-6(high)", wantThinking: `{"type":"enabled","budget_tokens":15999}`},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			base := thinking.ParseSuffix(tc.model).ModelName
			payload := fmt.Sprintf(`{"model":%q,"max_tokens":16000,"messages":[{"role":"user","content":"hi"}]}`, base)
			out := applyClaudeThinking(t, "claude", tc.model, payload)

			if got := gjson.GetBytes(out, "thinking").Raw; got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %q", got, tc.wantThinking)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("output_config.effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

// A Claude client that already speaks adaptive thinking is forwarded byte for
// byte, on the adaptive-only models and on Opus 4.6, which accepts it too.
func TestClaudeNativeAdaptiveRequestPassesThrough(t *testing.T) {
	bodies := []string{
		`{"model":"%s","max_tokens":16000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"%s","max_tokens":16000,"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"%s","max_tokens":16000,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`,
		// An effort this build does not list is the upstream's to judge.
		`{"model":"%s","max_tokens":16000,"thinking":{"type":"adaptive"},"output_config":{"effort":"ultra"},"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1", "claude-sonnet-5-5", "claude-opus-4-7", "claude-opus-4-6"} {
		for i, template := range bodies {
			t.Run(fmt.Sprintf("%s/%d", model, i), func(t *testing.T) {
				payload := fmt.Sprintf(template, model)
				out := applyClaudeThinking(t, "claude", model, payload)
				if string(out) != payload {
					t.Fatalf("body changed:\n got  %s\n want %s", out, payload)
				}
			})
		}
	}

	t.Run("between_tools on Sonnet 5.5", func(t *testing.T) {
		payload := `{"model":"claude-sonnet-5-5","max_tokens":16000,"thinking":{"type":"between_tools"},"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hi"}]}`
		if out := applyClaudeThinking(t, "claude", "claude-sonnet-5-5", payload); string(out) != payload {
			t.Fatalf("body changed:\n got  %s\n want %s", out, payload)
		}
	})
}

// Claude clients that still send the budget form or "disabled" get the shape
// the target model accepts.
func TestClaudeNativeLegacyThinkingIsRewrittenForAdaptiveModels(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		thinking     string
		effort       string
		wantThinking string
		wantEffort   string
	}{
		{name: "budget becomes effort", model: "claude-opus-5-5", thinking: `{"type":"enabled","budget_tokens":10000}`, wantThinking: adaptiveSummarized, wantEffort: "high"},
		// Clients written for Opus 4.6 send both; effort is the only depth
		// control left, so the one the client chose stays.
		{name: "explicit effort beats the budget", model: "claude-opus-5-5", thinking: `{"type":"enabled","budget_tokens":1024}`, effort: "max", wantThinking: adaptiveSummarized, wantEffort: "max"},
		{name: "explicit effort beats the budget on 4.7", model: "claude-opus-4-7", thinking: `{"type":"enabled","budget_tokens":30000}`, effort: "low", wantThinking: adaptiveSummarized, wantEffort: "low"},
		{name: "budget keeps chosen display", model: "claude-opus-4-8", thinking: `{"type":"enabled","budget_tokens":2000,"display":"omitted"}`, wantThinking: `{"type":"adaptive","display":"omitted"}`, wantEffort: "medium"},
		{name: "enabled without budget is adaptive", model: "claude-fable-5", thinking: `{"type":"enabled"}`, wantThinking: adaptiveSummarized},
		{name: "always-on model keeps the client's effort", model: "claude-opus-5-5", thinking: `{"type":"disabled"}`, effort: "max", wantEffort: "max"},
		{name: "always-on model without effort", model: "claude-fable-5-1", thinking: `{"type":"disabled"}`, wantEffort: "low"},
		{name: "Opus 5 caps effort when disabled", model: "claude-opus-5", thinking: `{"type":"disabled"}`, effort: "max", wantThinking: `{"type":"disabled"}`, wantEffort: "high"},
		{name: "Opus 5 below the cap", model: "claude-opus-5", thinking: `{"type":"disabled"}`, effort: "medium", wantThinking: `{"type":"disabled"}`, wantEffort: "medium"},
		{name: "Opus 4.7 disables at any effort", model: "claude-opus-4-7", thinking: `{"type":"disabled"}`, effort: "max", wantThinking: `{"type":"disabled"}`, wantEffort: "max"},
		{name: "Sonnet 5.5 uses between_tools", model: "claude-sonnet-5-5", thinking: `{"type":"disabled"}`, effort: "xhigh", wantThinking: `{"type":"between_tools"}`, wantEffort: "high"},
		{name: "between_tools elsewhere is disabled", model: "claude-sonnet-5", thinking: `{"type":"between_tools"}`, wantThinking: `{"type":"disabled"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"model":%q,"max_tokens":16000,"messages":[{"role":"user","content":"hi"}]}`, tc.model)
			payload, _ = sjson.SetRaw(payload, "thinking", tc.thinking)
			if tc.effort != "" {
				payload, _ = sjson.Set(payload, "output_config.effort", tc.effort)
			}
			out := applyClaudeThinking(t, "claude", tc.model, payload)

			if got := gjson.GetBytes(out, "thinking").Raw; got != tc.wantThinking {
				t.Fatalf("thinking = %s, want %q", got, tc.wantThinking)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != tc.wantEffort {
				t.Fatalf("output_config.effort = %q, want %q", got, tc.wantEffort)
			}
		})
	}
}

// A configured Claude API key model is user-defined, but when its upstream name
// is an adaptive-only model the static catalog's rules come along with it
// (service registration copies Thinking from the catalog), and they apply.
func TestClaudeConfiguredModelFollowsItsUpstreamThinkingRules(t *testing.T) {
	static := registry.LookupStaticModelInfo("claude-opus-5-5")
	if static == nil || static.Thinking == nil {
		t.Fatal("claude-opus-5-5 is missing from the static catalog")
	}
	reg := registry.GetGlobalRegistry()
	uid := fmt.Sprintf("claude-adaptive-configured-%d", time.Now().UnixNano())
	reg.RegisterClient(uid, "claude", []*registry.ModelInfo{
		{ID: "team-opus", Object: "model", OwnedBy: "anthropic", Type: "claude", UpstreamModelID: "claude-opus-5-5", UserDefined: true, Thinking: static.Thinking},
		{ID: "team-unknown", Object: "model", OwnedBy: "anthropic", Type: "claude", UserDefined: true},
	})
	defer reg.UnregisterClient(uid)

	t.Run("level", func(t *testing.T) {
		from, payload := reasoningPayload("chat", "team-opus", "xhigh")
		out := applyClaudeThinking(t, from, "team-opus", payload)
		if got := gjson.GetBytes(out, "thinking").Raw; got != adaptiveSummarized {
			t.Fatalf("thinking = %s, want %s", got, adaptiveSummarized)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "xhigh" {
			t.Fatalf("output_config.effort = %q, want xhigh", got)
		}
	})
	t.Run("none", func(t *testing.T) {
		from, payload := reasoningPayload("responses", "team-opus", "none")
		out := applyClaudeThinking(t, from, "team-opus", payload)
		if got := gjson.GetBytes(out, "thinking"); got.Exists() {
			t.Fatalf("thinking = %s, want it omitted", got.Raw)
		}
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "low" {
			t.Fatalf("output_config.effort = %q, want low", got)
		}
	})
	t.Run("suffix below the listed levels", func(t *testing.T) {
		payload := `{"model":"team-opus","max_tokens":16000,"messages":[{"role":"user","content":"hi"}]}`
		out := applyClaudeThinking(t, "claude", "team-opus(minimal)", payload)
		if got := gjson.GetBytes(out, "output_config.effort").String(); got != "low" {
			t.Fatalf("output_config.effort = %q, want low", got)
		}
	})
	t.Run("native adaptive passes through", func(t *testing.T) {
		payload := `{"model":"team-opus","max_tokens":16000,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`
		if out := applyClaudeThinking(t, "claude", "team-opus", payload); string(out) != payload {
			t.Fatalf("body changed:\n got  %s\n want %s", out, payload)
		}
	})

	// A name the catalog does not know keeps the budget form; the executor's
	// retry covers it if the upstream turns out to be adaptive-only. The
	// budget still has to stay below max_tokens.
	for _, level := range []string{"xhigh", "max"} {
		t.Run("unknown/"+level, func(t *testing.T) {
			from, payload := reasoningPayload("chat", "team-unknown", level)
			out := applyClaudeThinking(t, from, "team-unknown", payload)
			if got := gjson.GetBytes(out, "thinking").Raw; got != `{"type":"enabled","budget_tokens":31999}` {
				t.Fatalf("thinking = %s, want a budget below max_tokens", got)
			}
		})
	}
	t.Run("unknown native adaptive passes through", func(t *testing.T) {
		payload := `{"model":"team-unknown","max_tokens":16000,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hi"}]}`
		if out := applyClaudeThinking(t, "claude", "team-unknown", payload); string(out) != payload {
			t.Fatalf("body changed:\n got  %s\n want %s", out, payload)
		}
	})
}

package openai

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

// Command Code answers 400 with
// `expected one of "low"|"medium"|"high"|"xhigh"|"max"` when reasoning_effort
// carries one of this codebase's internal markers. Every Claude Code request
// that disabled thinking hit it, because the Claude→OpenAI translator writes
// `reasoning_effort: "none"` and nothing downstream took it back out.
func TestCompatibleModelNeverSendsInternalEffortMarkers(t *testing.T) {
	applier := NewApplier()
	userDefined := &registry.ModelInfo{ID: "deepseek/deepseek-v4-flash", UserDefined: true}

	cases := []struct {
		name   string
		body   string
		config thinking.ThinkingConfig
	}{
		{
			name:   "mode none",
			body:   `{"model":"deepseek/deepseek-v4-flash","reasoning_effort":"none"}`,
			config: thinking.ThinkingConfig{Mode: thinking.ModeNone, Budget: 0},
		},
		{
			name:   "mode auto",
			body:   `{"model":"deepseek/deepseek-v4-flash"}`,
			config: thinking.ThinkingConfig{Mode: thinking.ModeAuto, Budget: -1},
		},
		{
			name:   "zero budget maps to none",
			body:   `{"model":"deepseek/deepseek-v4-flash"}`,
			config: thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 0},
		},
		{
			name:   "negative budget maps to auto",
			body:   `{"model":"deepseek/deepseek-v4-flash"}`,
			config: thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: -1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := applier.Apply([]byte(tc.body), tc.config, userDefined)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if effort := gjson.GetBytes(out, "reasoning_effort"); effort.Exists() {
				t.Fatalf("reasoning_effort = %q, want the field to be absent", effort.String())
			}
			// The rest of the request must survive untouched.
			if model := gjson.GetBytes(out, "model").String(); model != "deepseek/deepseek-v4-flash" {
				t.Fatalf("model = %q, want it preserved", model)
			}
		})
	}
}

// Legitimate levels still have to reach the upstream — the guard must not turn
// into a blanket strip.
func TestCompatibleModelForwardsRealLevels(t *testing.T) {
	applier := NewApplier()
	userDefined := &registry.ModelInfo{ID: "deepseek/deepseek-v4-flash", UserDefined: true}

	for _, level := range []string{"low", "medium", "high", "xhigh", "max", "minimal"} {
		t.Run(level, func(t *testing.T) {
			out, err := applier.Apply(
				[]byte(`{"model":"deepseek/deepseek-v4-flash"}`),
				thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.ThinkingLevel(level)},
				userDefined,
			)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if got := gjson.GetBytes(out, "reasoning_effort").String(); got != level {
				t.Fatalf("reasoning_effort = %q, want %q", got, level)
			}
		})
	}
}

// A budget that maps onto a real level keeps working.
func TestCompatibleModelForwardsMappedBudget(t *testing.T) {
	out, err := NewApplier().Apply(
		[]byte(`{"model":"deepseek/deepseek-v4-flash"}`),
		thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 32000},
		&registry.ModelInfo{ID: "deepseek/deepseek-v4-flash", UserDefined: true},
	)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got == "" || got == "none" || got == "auto" {
		t.Fatalf("reasoning_effort = %q, want a real level", got)
	}
}

// A model that declares no thinking support is not a licence to forward a value
// written before this applier ran.
func TestModelWithoutThinkingSupportStripsInternalMarkers(t *testing.T) {
	applier := NewApplier()
	noThinking := &registry.ModelInfo{ID: "some-chat-model"}

	for _, marker := range []string{"none", "auto"} {
		t.Run(marker, func(t *testing.T) {
			body := []byte(`{"model":"some-chat-model","reasoning_effort":"` + marker + `"}`)
			out, err := applier.Apply(body, thinking.ThinkingConfig{Mode: thinking.ModeNone}, noThinking)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if effort := gjson.GetBytes(out, "reasoning_effort"); effort.Exists() {
				t.Fatalf("reasoning_effort = %q, want the field to be absent", effort.String())
			}
		})
	}

	// A real level on the same path is left alone.
	body := []byte(`{"model":"some-chat-model","reasoning_effort":"high"}`)
	out, err := applier.Apply(body, thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.LevelHigh}, noThinking)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want high", got)
	}
}

// Models that explicitly declare "none" keep receiving it: the fix targets
// unknown capability, not models that opted in.
func TestDeclaredNoneLevelIsStillHonoured(t *testing.T) {
	declared := &registry.ModelInfo{
		ID:       "gpt-5.1",
		Thinking: &registry.ThinkingSupport{Levels: []string{"none", "low", "medium", "high"}},
	}
	out, err := NewApplier().Apply(
		[]byte(`{"model":"gpt-5.1"}`),
		thinking.ThinkingConfig{Mode: thinking.ModeNone},
		declared,
	)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "none" {
		t.Fatalf("reasoning_effort = %q, want none for a model that declares it", got)
	}
}

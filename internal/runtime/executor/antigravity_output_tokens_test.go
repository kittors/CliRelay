package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestClampAntigravityOutputTokens_CapsClaudeAboveCeiling(t *testing.T) {
	// The exact payload shape that failed: an agent client asking for 65536.
	payload := `{"request":{"generationConfig":{"maxOutputTokens":65536,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":24576}}}}`

	got := clampAntigravityOutputTokens(payload, "claude-opus-4-6-thinking")

	if v := gjson.Get(got, "request.generationConfig.maxOutputTokens").Int(); v != antigravityClaudeMaxOutputTokens {
		t.Fatalf("maxOutputTokens = %d, want %d", v, antigravityClaudeMaxOutputTokens)
	}
	// A budget already under the cap is the client's choice; leave it alone.
	if v := gjson.Get(got, "request.generationConfig.thinkingConfig.thinkingBudget").Int(); v != 24576 {
		t.Fatalf("thinkingBudget = %d, want it untouched at 24576", v)
	}
	if v := gjson.Get(got, "request.generationConfig.thinkingConfig.includeThoughts").Bool(); !v {
		t.Fatal("includeThoughts was dropped")
	}
}

func TestClampAntigravityOutputTokens_LeavesClaudeAtOrBelowCeiling(t *testing.T) {
	for _, value := range []int64{1, 8192, antigravityClaudeMaxOutputTokens} {
		payload := `{"request":{"generationConfig":{"maxOutputTokens":` + itoa(value) + `}}}`

		got := clampAntigravityOutputTokens(payload, "claude-sonnet-4-6")

		if got != payload {
			t.Fatalf("maxOutputTokens %d was rewritten: %s", value, got)
		}
	}
}

func TestClampAntigravityOutputTokens_KeepsThinkingBudgetUnderTheCap(t *testing.T) {
	// A budget that cleared the client's own ceiling can exceed ours. Trading
	// the bare 400 for "`max_tokens` must be greater than `thinking.budget_tokens`"
	// would be no fix at all.
	payload := `{"request":{"generationConfig":{"maxOutputTokens":70000,"thinkingConfig":{"thinkingBudget":65000}}}}`

	got := clampAntigravityOutputTokens(payload, "claude-opus-4-6-thinking")

	maxTokens := gjson.Get(got, "request.generationConfig.maxOutputTokens").Int()
	budget := gjson.Get(got, "request.generationConfig.thinkingConfig.thinkingBudget").Int()
	if maxTokens != antigravityClaudeMaxOutputTokens {
		t.Fatalf("maxOutputTokens = %d, want %d", maxTokens, antigravityClaudeMaxOutputTokens)
	}
	if budget >= maxTokens {
		t.Fatalf("thinkingBudget %d must stay below maxOutputTokens %d", budget, maxTokens)
	}
}

func TestClampAntigravityOutputTokens_LeavesOtherFamiliesAlone(t *testing.T) {
	// Gemini answers 200 at 65536, and the GPT family has its maxOutputTokens
	// removed upstream of this call. Neither should be rewritten here.
	payload := `{"request":{"generationConfig":{"maxOutputTokens":65536}}}`

	for _, model := range []string{"gemini-3-flash", "gpt-oss-120b-medium", "some-new-model"} {
		if got := clampAntigravityOutputTokens(payload, model); got != payload {
			t.Fatalf("model %s was rewritten: %s", model, got)
		}
	}
}

func TestClampAntigravityOutputTokens_IgnoresPayloadWithoutTheField(t *testing.T) {
	payload := `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`

	if got := clampAntigravityOutputTokens(payload, "claude-opus-4-6-thinking"); got != payload {
		t.Fatalf("payload without maxOutputTokens was rewritten: %s", got)
	}
}

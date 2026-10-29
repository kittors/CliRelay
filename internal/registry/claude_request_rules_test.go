package registry

import "testing"

func claudeRequestRulesOf(model string) ClaudeRequestRules {
	if info := LookupClaudeModelInfo(model); info != nil && info.ClaudeRequest != nil {
		return *info.ClaudeRequest
	}
	return ClaudeRequestRules{}
}

func TestClaudeCatalogRequestRules(t *testing.T) {
	both := ClaudeRequestRules{NoSampling: true, NoForcedToolChoice: true}
	sampling := ClaudeRequestRules{NoSampling: true}
	cases := map[string]ClaudeRequestRules{
		"claude-opus-5-5":           both,
		"claude-sonnet-5-5":         both,
		"claude-fable-5-1":          both,
		"CLAUDE-OPUS-5-5":           both,
		"claude-opus-5":             sampling,
		"claude-sonnet-5":           sampling,
		"claude-fable-5":            sampling,
		"claude-opus-4-8":           sampling,
		"claude-opus-4-7":           sampling,
		"claude-opus-4-6":           {},
		"claude-sonnet-4-6":         {},
		"claude-haiku-4-5-20251001": {},
		"claude-opus-6":             {},
		"":                          {},
	}
	for model, want := range cases {
		if got := claudeRequestRulesOf(model); got != want {
			t.Errorf("rules of %q = %+v, want %+v", model, got, want)
		}
	}
}

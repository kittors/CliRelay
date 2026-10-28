package registry

import (
	"slices"
	"testing"
)

// The per-model thinking rules from Anthropic's model table. A wrong entry is a
// 400 for every request that turns thinking off on that model.
func TestClaudeAdaptiveModelsDeclareTheirOffSwitch(t *testing.T) {
	cases := []struct {
		id          string
		off         string
		offMaxLevel string
	}{
		{id: "claude-opus-5-5", off: ClaudeThinkingAlwaysOn},
		{id: "claude-fable-5-1", off: ClaudeThinkingAlwaysOn},
		{id: "claude-fable-5", off: ClaudeThinkingAlwaysOn},
		{id: "claude-opus-5", off: ClaudeThinkingOffDisabled, offMaxLevel: "high"},
		{id: "claude-sonnet-5-5", off: ClaudeThinkingOffBetweenTools, offMaxLevel: "high"},
		{id: "claude-sonnet-5", off: ClaudeThinkingOffDisabled},
		{id: "claude-opus-4-8", off: ClaudeThinkingOffDisabled},
		{id: "claude-opus-4-7", off: ClaudeThinkingOffDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			info := LookupStaticModelInfo(tc.id)
			if info == nil || info.Thinking == nil {
				t.Fatalf("LookupStaticModelInfo(%q) has no thinking support", tc.id)
			}
			support := info.Thinking
			if support.Adaptive == nil {
				t.Fatal("Adaptive is nil; budgets would be sent to a model that rejects them")
			}
			if support.Adaptive.Off != tc.off || support.Adaptive.OffMaxLevel != tc.offMaxLevel {
				t.Fatalf("Adaptive = %+v, want off=%q off_max_level=%q", *support.Adaptive, tc.off, tc.offMaxLevel)
			}
			// Level-only: a translator's budget must be converted, not sent.
			if support.Min != 0 || support.Max != 0 {
				t.Fatalf("Min/Max = %d/%d, want 0/0", support.Min, support.Max)
			}
			if !slices.Equal(support.Levels, []string{"low", "medium", "high", "xhigh", "max"}) {
				t.Fatalf("Levels = %v", support.Levels)
			}
			if !support.DynamicAllowed {
				t.Fatal("DynamicAllowed = false; auto would be pinned to a level instead of left to the model")
			}
			// /v1/models offers "none" exactly when the model can turn thinking off.
			if want := tc.off != ClaudeThinkingAlwaysOn; support.ZeroAllowed != want {
				t.Fatalf("ZeroAllowed = %v, want %v", support.ZeroAllowed, want)
			}
		})
	}
}

// Opus 4.6 and Sonnet 4.6 still accept budgets and stay on them.
func TestClaudeBudgetModelsAreNotAdaptive(t *testing.T) {
	for _, id := range []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-haiku-4-5-20251001"} {
		info := LookupStaticModelInfo(id)
		if info == nil || info.Thinking == nil {
			t.Fatalf("LookupStaticModelInfo(%q) has no thinking support", id)
		}
		if info.Thinking.Adaptive != nil {
			t.Errorf("%s is marked adaptive", id)
		}
		if info.Thinking.Min != 1024 || info.Thinking.Max != 128000 {
			t.Errorf("%s budget range = %d..%d, want 1024..128000", id, info.Thinking.Min, info.Thinking.Max)
		}
	}
}

// Claude credentials register their models through the SDK catalog and back;
// losing Adaptive on the way would send budgets again.
func TestAdaptiveThinkingSurvivesTheSDKBridge(t *testing.T) {
	for _, channel := range []string{"claude", "bedrock"} {
		checked := 0
		for _, model := range ModelInfosFromSDK(ModelInfosToSDK(GetStaticModelDefinitionsByChannel(channel))) {
			static := LookupStaticModelInfo(model.ID)
			if static == nil || static.Thinking == nil || static.Thinking.Adaptive == nil {
				continue
			}
			checked++
			if model.Thinking == nil || model.Thinking.Adaptive == nil {
				t.Errorf("%s/%s lost Adaptive in the round trip", channel, model.ID)
				continue
			}
			if *model.Thinking.Adaptive != *static.Thinking.Adaptive {
				t.Errorf("%s/%s Adaptive = %+v, want %+v", channel, model.ID, *model.Thinking.Adaptive, *static.Thinking.Adaptive)
			}
			if model.Thinking.Adaptive == static.Thinking.Adaptive {
				t.Errorf("%s/%s shares its Adaptive pointer with the catalog", channel, model.ID)
			}
		}
		if checked == 0 {
			t.Errorf("channel %s lists no adaptive model; this test would prove nothing", channel)
		}
	}
}

package api

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// /v1/models offers "none" only for Claude models that can turn thinking off;
// Opus 5.5 and Fable reject every way of doing so.
func TestStaticReasoningLevelsForClaudeModels(t *testing.T) {
	cases := map[string][]string{
		"claude-opus-5-5":   {"low", "medium", "high", "xhigh", "max"},
		"claude-fable-5-1":  {"low", "medium", "high", "xhigh", "max"},
		"claude-sonnet-5-5": {"none", "low", "medium", "high", "xhigh", "max"},
		"claude-opus-4-7":   {"none", "low", "medium", "high", "xhigh", "max"},
		"claude-opus-4-6":   {"none", "low", "medium", "high"},
	}
	for id, want := range cases {
		info := registry.LookupStaticModelInfo(id)
		if info == nil {
			t.Fatalf("LookupStaticModelInfo(%q) = nil", id)
		}
		if got := staticReasoningLevels(info); !slices.Equal(got, want) {
			t.Errorf("staticReasoningLevels(%s) = %v, want %v", id, got, want)
		}
	}
}

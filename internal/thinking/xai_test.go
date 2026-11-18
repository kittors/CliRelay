package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/xai"
	"github.com/tidwall/gjson"
)

func TestXAIThinkingUsesCodexReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"auto"}}`)

	got, err := thinking.ApplyThinking(body, "grok-4.3", "xai", "xai", "xai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "medium" {
		t.Fatalf("reasoning.effort = %q, want medium; body=%s", effort, got)
	}
}

func TestXAIThinkingStripsUnsupportedModelConfig(t *testing.T) {
	body := []byte(`{"model":"grok-build-0.1","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high"}}`)

	got, err := thinking.ApplyThinking(body, "grok-build-0.1", "xai", "xai", "xai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be stripped; body=%s", got)
	}
}

// grok-4.5 rejects effort=none; clamp disable to lowest supported level (low).
func TestXAIGrok45ModeNoneClampsToLowestLevel(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"none"}}`)

	got, err := thinking.ApplyThinking(body, "grok-4.5", "xai", "xai", "xai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "low" {
		t.Fatalf("reasoning.effort = %q, want low; body=%s", effort, got)
	}
}

func TestXAIGrok43ModeNoneKeepsNoneEffort(t *testing.T) {
	body := []byte(`{"model":"grok-4.3","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"none"}}`)

	got, err := thinking.ApplyThinking(body, "grok-4.3", "xai", "xai", "xai")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "none" {
		t.Fatalf("reasoning.effort = %q, want none; body=%s", effort, got)
	}
}

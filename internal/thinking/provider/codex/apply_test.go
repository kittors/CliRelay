package codex

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyThinkingSupportsMaxButRejectsUltraWireEffort(t *testing.T) {
	maxBody := []byte(`{"model":"gpt-5.6-sol","reasoning":{"effort":"max"}}`)
	got, err := thinking.ApplyThinking(maxBody, "gpt-5.6-sol", "codex", "codex", "codex")
	if err != nil {
		t.Fatalf("ApplyThinking(max) error: %v", err)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", effort, got)
	}

	ultraBody := []byte(`{"model":"gpt-5.6-sol","reasoning":{"effort":"ultra"}}`)
	if _, err := thinking.ApplyThinking(ultraBody, "gpt-5.6-sol", "codex", "codex", "codex"); err == nil {
		t.Fatal("ApplyThinking(ultra) error = nil, want unsupported wire effort error")
	}
}

func TestApplyThinkingMaxSuffix(t *testing.T) {
	got, err := thinking.ApplyThinking([]byte(`{"model":"gpt-5.6-sol"}`), "gpt-5.6-sol(max)", "codex", "codex", "codex")
	if err != nil {
		t.Fatalf("ApplyThinking(max suffix) error: %v", err)
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "max" {
		t.Fatalf("reasoning.effort = %q, want max; body=%s", effort, got)
	}
}

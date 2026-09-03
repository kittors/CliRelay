package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestApplyCodexServiceTierPolicy(t *testing.T) {
	baseBody := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)

	t.Run("default strips service_tier", func(t *testing.T) {
		clientBody := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello"}`)
		out := applyCodexServiceTierPolicy(baseBody, clientBody, nil)
		if gjson.GetBytes(out, "service_tier").Exists() {
			t.Fatalf("expected service_tier to be stripped, got %v", gjson.GetBytes(out, "service_tier").String())
		}
	})

	t.Run("drop explicitly strips service_tier", func(t *testing.T) {
		auth := &cliproxyauth.Auth{
			Metadata: map[string]any{"codex_service_tier": "drop"},
		}
		clientBody := []byte(`{"model":"gpt-5.5","service_tier":"priority","input":"hello"}`)
		out := applyCodexServiceTierPolicy(baseBody, clientBody, auth)
		if gjson.GetBytes(out, "service_tier").Exists() {
			t.Fatalf("expected service_tier to be stripped, got %v", gjson.GetBytes(out, "service_tier").String())
		}
	})

	t.Run("priority forces priority tier", func(t *testing.T) {
		auth := &cliproxyauth.Auth{
			Metadata: map[string]any{"codex_service_tier": "priority"},
		}
		out := applyCodexServiceTierPolicy(baseBody, nil, auth)
		if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
			t.Fatalf("expected service_tier=priority, got %q", got)
		}
	})

	t.Run("flex forces flex tier", func(t *testing.T) {
		auth := &cliproxyauth.Auth{
			Attributes: map[string]string{"codex_service_tier": "flex"},
		}
		out := applyCodexServiceTierPolicy(baseBody, nil, auth)
		if got := gjson.GetBytes(out, "service_tier").String(); got != "flex" {
			t.Fatalf("expected service_tier=flex, got %q", got)
		}
	})

	t.Run("pass passes client fast as priority", func(t *testing.T) {
		auth := &cliproxyauth.Auth{
			Metadata: map[string]any{"codex_service_tier": "pass"},
		}
		clientBody := []byte(`{"model":"gpt-5.5","service_tier":"fast","input":"hello"}`)
		out := applyCodexServiceTierPolicy(baseBody, clientBody, auth)
		if got := gjson.GetBytes(out, "service_tier").String(); got != "priority" {
			t.Fatalf("expected service_tier=priority when pass receives fast, got %q", got)
		}
	})

	t.Run("pass drops unknown/invalid client tier", func(t *testing.T) {
		auth := &cliproxyauth.Auth{
			Metadata: map[string]any{"codex_service_tier": "pass"},
		}
		clientBody := []byte(`{"model":"gpt-5.5","service_tier":"turbo","input":"hello"}`)
		out := applyCodexServiceTierPolicy(baseBody, clientBody, auth)
		if gjson.GetBytes(out, "service_tier").Exists() {
			t.Fatalf("expected unknown tier to be dropped, got %v", gjson.GetBytes(out, "service_tier").String())
		}
	})
}

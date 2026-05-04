package synthesizer

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestFileSynthesizer_Synthesize_CodexFreeTierDoesNotInjectExcludedModels(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":       "codex",
		"email":      "free@example.com",
		"plan_type":  "free",
		"account_id": "acct_123",
	}
	data, _ := json.Marshal(authData)
	if err := os.WriteFile(filepath.Join(tempDir, "codex.json"), data, 0o644); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	got := auths[0].Attributes["excluded_models"]
	if got != "" {
		t.Fatalf("expected no excluded_models for free tier, got %q", got)
	}
}

func TestFileSynthesizer_Synthesize_CodexBackfillsPlanTypeFromIDToken(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":       "codex",
		"email":      "free@example.com",
		"account_id": "acct_123",
		"id_token":   makeCodexJWTForTest(t, "FREE", "acct_123", "free@example.com"),
	}
	data, _ := json.Marshal(authData)
	if err := os.WriteFile(filepath.Join(tempDir, "codex.json"), data, 0o644); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if got, _ := auths[0].Metadata["plan_type"].(string); got != "free" {
		t.Fatalf("expected plan_type free, got %q", got)
	}
	if got := auths[0].Attributes["excluded_models"]; got != "" {
		t.Fatalf("expected no excluded_models for free tier, got %q", got)
	}
}

func makeCodexJWTForTest(t *testing.T, planType, accountID, email string) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	claims := map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  planType,
			"chatgpt_account_id": accountID,
		},
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}

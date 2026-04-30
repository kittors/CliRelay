package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestCodexBuildAuthRecord_PersistsPlanTypeMetadata(t *testing.T) {
	a := NewCodexAuthenticator()
	authSvc := codex.NewCodexAuth(&config.Config{})

	claims := map[string]any{
		"email": "free@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "FREE",
			"chatgpt_account_id": "acct_123",
		},
	}
	jwt := makeJWTForTest(t, claims)
	bundle := &codex.CodexAuthBundle{
		TokenData: codex.CodexTokenData{
			IDToken:      jwt,
			AccessToken:  "token",
			RefreshToken: "refresh",
			AccountID:    "acct_123",
			Email:        "free@example.com",
		},
	}

	record, err := a.buildAuthRecord(authSvc, bundle)
	if err != nil {
		t.Fatalf("buildAuthRecord() error = %v", err)
	}
	if got := record.Metadata["plan_type"]; got != "free" {
		t.Fatalf("plan_type = %v, want free", got)
	}
	if got := record.Metadata["account_id"]; got != "acct_123" {
		t.Fatalf("account_id = %v, want acct_123", got)
	}
}

func makeJWTForTest(t *testing.T, claims map[string]any) string {
	t.Helper()
	encode := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal jwt part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(claims) + ".sig"
}

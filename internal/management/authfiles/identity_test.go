package authfiles

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestEmailPrefersMetadataThenAttributes(t *testing.T) {
	auth := &coreauth.Auth{
		Metadata: map[string]any{"email": " metadata@example.com "},
		Attributes: map[string]string{
			"email": "attribute@example.com",
		},
	}
	if got := Email(auth); got != "metadata@example.com" {
		t.Fatalf("Email() = %q, want metadata@example.com", got)
	}

	delete(auth.Metadata, "email")
	if got := Email(auth); got != "attribute@example.com" {
		t.Fatalf("Email() = %q, want attribute@example.com", got)
	}
}

func TestIsRuntimeOnlyUsesRuntimeOnlyAttribute(t *testing.T) {
	auth := &coreauth.Auth{Attributes: map[string]string{"runtime_only": " true "}}
	if !IsRuntimeOnly(auth) {
		t.Fatal("IsRuntimeOnly() = false, want true")
	}
}

func TestCodexIDTokenClaimsReturnsDisplayClaims(t *testing.T) {
	idToken := makeJWTForTest(t, map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "plus",
			"chatgpt_account_id": "acct_123",
			"chatgpt_user_id":    "user_team_member",
		},
	})
	auth := &coreauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"id_token": idToken},
	}

	claims := CodexIDTokenClaims(auth)
	if claims == nil {
		t.Fatal("expected id token claims")
	}
	if got, _ := claims["plan_type"].(string); got != "plus" {
		t.Fatalf("plan_type = %q, want plus", got)
	}
	if got, _ := claims["chatgpt_account_id"].(string); got != "acct_123" {
		t.Fatalf("chatgpt_account_id = %q, want acct_123", got)
	}
	if got, _ := claims["chatgpt_user_id"].(string); got != "user_team_member" {
		t.Fatalf("chatgpt_user_id = %q, want user_team_member", got)
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

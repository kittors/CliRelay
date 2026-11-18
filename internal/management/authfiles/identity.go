package authfiles

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Email returns the display email stored in auth metadata or attributes.
func Email(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if email := MetadataString(auth.Metadata, "email"); email != "" {
		return email
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

// Attribute returns an auth attribute value without exposing the attributes map
// to callers that only need read-only display data.
func Attribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

// IsRuntimeOnly reports whether an auth entry exists only in memory and should
// not require a backing file path.
func IsRuntimeOnly(auth *coreauth.Auth) bool {
	return strings.EqualFold(strings.TrimSpace(Attribute(auth, "runtime_only")), "true")
}

// CodexIDTokenClaims extracts the user-facing Codex id-token claims shown by
// the management auth-file response.
func CodexIDTokenClaims(auth *coreauth.Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return nil
	}

	result := map[string]any{}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptUserID); v != "" {
		result["chatgpt_user_id"] = v
	}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
		result["plan_type"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
		result["chatgpt_subscription_active_until"] = v
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

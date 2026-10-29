package authfiles

import (
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// CodexOAuthHealth is the public view of a Codex account's sign-in health: whether
// its token is being renewed or the account has to be signed in again. It is
// sanitized the same way the Claude OAuth health is.
func CodexOAuthHealth(auth *coreauth.Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	health := sanitizeClaudeOAuthHealthValue(auth.Metadata[coreauth.CodexOAuthHealthMetadataKey], 0)
	asMap, ok := health.(map[string]any)
	if !ok || len(asMap) == 0 {
		return nil
	}
	return asMap
}

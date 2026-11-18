package auth

import (
	"maps"
	"strings"
)

// InferAuthProvider returns the explicit provider type, or derives it from
// supported OAuth credential metadata.
func InferAuthProvider(metadata map[string]any) string {
	provider := metadataString(metadata, "type")
	if provider == "" && isCodexOAuthMetadata(metadata) {
		provider = "codex"
		if metadata != nil {
			metadata["type"] = provider
		}
	}
	if provider == "" {
		return "unknown"
	}
	return provider
}

func isCodexOAuthMetadata(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	hasAccount := metadataString(metadata, "chatgpt_account_id", "chatgptAccountID", "account_id") != ""
	hasToken := metadataString(metadata, "access_token", "id_token", "refresh_token", "session_token") != ""
	hasOpenAIClient := strings.HasPrefix(metadataString(metadata, "client_id"), "app_")
	hasCodexClaims := metadataString(metadata, "chatgpt_plan_type", "account_claims_email") != ""
	return hasToken && (hasAccount || hasOpenAIClient || hasCodexClaims)
}

// NormalizeAuthMetadata fills canonical fields expected by provider executors
// while preserving source-specific metadata.
func NormalizeAuthMetadata(metadata map[string]any, provider string) map[string]any {
	if len(metadata) == 0 {
		return metadata
	}
	normalized := maps.Clone(metadata)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		normalizeCodexAuthMetadata(normalized)
	case "xai", "x-ai", "grok":
		normalizeXAIAuthMetadata(normalized)
	case "claude", "anthropic":
		normalizeClaudeAuthMetadata(normalized)
	case "kimi":
		normalizeKimiAuthMetadata(normalized)
	case "gemini-cli", "gemini", "antigravity":
		normalizeGeminiFamilyAuthMetadata(normalized, provider)
	}
	return normalized
}

func normalizeXAIAuthMetadata(metadata map[string]any) {
	metadata["type"] = "xai"
	if metadataString(metadata, "auth_kind", "authKind") == "" {
		switch {
		case metadataString(metadata, "refresh_token", "refreshToken", "access_token", "accessToken") != "":
			metadata["auth_kind"] = "oauth"
		case metadataString(metadata, "api_key", "apiKey") != "":
			metadata["auth_kind"] = "api_key"
		}
	}
	if metadataString(metadata, "base_url", "baseUrl") == "" {
		metadata["base_url"] = "https://api.x.ai/v1"
	}
	if metadataString(metadata, "token_endpoint", "tokenEndpoint") == "" {
		metadata["token_endpoint"] = "https://auth.x.ai/oauth2/token"
	}
	normalizeXAIMetadataFromJWT(metadataString(metadata, "access_token", "accessToken"), metadata)
	normalizeXAIMetadataFromJWT(metadataString(metadata, "id_token", "idToken"), metadata)
	if _, ok := metadata["using_api"]; !ok && strings.EqualFold(metadataString(metadata, "auth_kind"), "oauth") {
		metadata["using_api"] = false
	}
}

func normalizeXAIMetadataFromJWT(token string, metadata map[string]any) {
	claims, ok := parseJWTClaimsMap(token)
	if !ok {
		return
	}
	if metadataString(metadata, "sub") == "" {
		if value, ok := claims["sub"].(string); ok && strings.TrimSpace(value) != "" {
			metadata["sub"] = strings.TrimSpace(value)
		}
	}
	if metadataString(metadata, "email") == "" {
		if value, ok := claims["email"].(string); ok && strings.TrimSpace(value) != "" {
			metadata["email"] = strings.TrimSpace(value)
		}
	}
	if metadataString(metadata, "expired") == "" {
		if exp, ok := jwtNumericClaim(claims, "exp"); ok && exp > 0 {
			metadata["expired"] = exp
		}
	}
}

func normalizeClaudeAuthMetadata(metadata map[string]any) {
	if metadataString(metadata, "type") == "" {
		metadata["type"] = "claude"
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" &&
		metadataString(metadata, "refresh_token", "refreshToken", "access_token", "accessToken") != "" {
		metadata["auth_kind"] = "oauth"
	}
}

func normalizeKimiAuthMetadata(metadata map[string]any) {
	if metadataString(metadata, "type") == "" {
		metadata["type"] = "kimi"
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" &&
		metadataString(metadata, "refresh_token", "refreshToken", "access_token", "accessToken") != "" {
		metadata["auth_kind"] = "oauth"
	}
}

func normalizeGeminiFamilyAuthMetadata(metadata map[string]any, provider string) {
	if metadataString(metadata, "type") == "" {
		metadata["type"] = strings.ToLower(strings.TrimSpace(provider))
	}
	if metadataString(metadata, "auth_kind", "authKind") == "" &&
		(metadataString(metadata, "refresh_token", "refreshToken") != "" || hasNestedRefreshToken(metadata)) {
		metadata["auth_kind"] = "oauth"
	}
}

func hasNestedRefreshToken(metadata map[string]any) bool {
	token, ok := metadata["token"].(map[string]any)
	if !ok {
		return false
	}
	return metadataString(token, "refresh_token", "refreshToken") != ""
}

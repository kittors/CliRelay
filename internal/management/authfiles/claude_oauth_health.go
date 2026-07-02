package authfiles

import (
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func ClaudeOAuthHealth(auth *coreauth.Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	health := sanitizeClaudeOAuthHealthValue(auth.Metadata[coreauth.ClaudeOAuthHealthMetadataKey], 0)
	asMap, ok := health.(map[string]any)
	if !ok || len(asMap) == 0 {
		return nil
	}
	return asMap
}

func sanitizeClaudeOAuthHealthValue(value any, depth int) any {
	if depth > 8 {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			if isSensitiveClaudeOAuthHealthKey(key) {
				continue
			}
			if cloned := sanitizeClaudeOAuthHealthValue(val, depth+1); cloned != nil {
				out[key] = cloned
			}
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(typed))
		for key, val := range typed {
			if isSensitiveClaudeOAuthHealthKey(key) {
				continue
			}
			out[key] = val
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, val := range typed {
			if cloned := sanitizeClaudeOAuthHealthValue(val, depth+1); cloned != nil {
				out = append(out, cloned)
			}
		}
		return out
	case string:
		return typed
	case bool:
		return typed
	case int:
		return typed
	case int64:
		return typed
	case float64:
		return typed
	case time.Time:
		if typed.IsZero() {
			return nil
		}
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return nil
	}
}

func isSensitiveClaudeOAuthHealthKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	if normalized == "" {
		return true
	}
	return strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "api_key") ||
		normalized == "authorization" ||
		normalized == "cookie" ||
		normalized == "secret"
}

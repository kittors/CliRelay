package auth

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

func metadataString(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func normalizeCodexAuthMetadata(metadata map[string]any) {
	if metadata == nil {
		return
	}
	metadata["type"] = "codex"
	if accountID := metadataString(metadata, "account_id", "chatgpt_account_id", "chatgptAccountID"); accountID != "" {
		metadata["account_id"] = accountID
	}
	if email := metadataString(metadata, "email", "account_claims_email", "accountClaimsEmail"); email != "" {
		metadata["email"] = email
	}
	if planType := strings.ToLower(metadataString(metadata, "plan_type", "planType", "chatgpt_plan_type")); planType != "" {
		metadata["plan_type"] = planType
	}
	if userID := metadataString(metadata, "chatgpt_user_id", "chatgptUserId"); userID != "" {
		metadata["chatgpt_user_id"] = userID
	}
	normalizeCodexMetadataFromJWT(metadataString(metadata, "id_token"), metadata)
	normalizeCodexMetadataFromJWT(metadataString(metadata, "access_token"), metadata)
}

func normalizeCodexMetadataFromJWT(token string, metadata map[string]any) {
	claims, ok := parseJWTClaimsMap(token)
	if !ok {
		return
	}
	if metadataString(metadata, "email") == "" {
		if email, ok := claims["email"].(string); ok && strings.TrimSpace(email) != "" {
			metadata["email"] = strings.TrimSpace(email)
		}
	}
	if metadataString(metadata, "expired") == "" {
		if exp, ok := jwtNumericClaim(claims, "exp"); ok && exp > 0 {
			metadata["expired"] = time.Unix(exp, 0).UTC().Format(time.RFC3339)
		}
	}
	if metadataString(metadata, "last_refresh") == "" {
		if iat, ok := jwtNumericClaim(claims, "iat"); ok && iat > 0 {
			metadata["last_refresh"] = time.Unix(iat, 0).UTC().Format(time.RFC3339)
		}
	}
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if len(authClaims) == 0 {
		return
	}
	if metadataString(metadata, "account_id") == "" {
		if accountID := metadataString(authClaims, "account_id", "chatgpt_account_id"); accountID != "" {
			metadata["account_id"] = accountID
		}
	}
	if metadataString(metadata, "chatgpt_user_id") == "" {
		if userID := metadataString(authClaims, "chatgpt_user_id"); userID != "" {
			metadata["chatgpt_user_id"] = userID
		}
	}
	if metadataString(metadata, "plan_type") == "" {
		if planType := strings.ToLower(metadataString(authClaims, "chatgpt_plan_type", "plan_type")); planType != "" {
			metadata["plan_type"] = planType
		}
	}
}

func parseJWTClaimsMap(token string) (map[string]any, bool) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

func jwtNumericClaim(claims map[string]any, key string) (int64, bool) {
	switch value := claims[key].(type) {
	case float64:
		return int64(value), value > 0
	case int64:
		return value, value > 0
	case int:
		return int64(value), value > 0
	case json.Number:
		if i, err := value.Int64(); err == nil && i > 0 {
			return i, true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && i > 0 {
			return i, true
		}
	}
	return 0, false
}

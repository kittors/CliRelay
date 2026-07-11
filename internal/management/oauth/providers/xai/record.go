package xai

import (
	"strconv"
	"strings"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const xaiUsingAPIKey = "using_api"

func RecordFromTokenStorage(tokenStorage *internalxai.TokenStorage, now time.Time, usingAPI bool) *coreauth.Auth {
	if tokenStorage == nil || strings.TrimSpace(tokenStorage.AccessToken) == "" {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	fileName := internalxai.CredentialFileName(tokenStorage.Email, tokenStorage.Subject)
	label := strings.TrimSpace(tokenStorage.Email)
	if label == "" {
		label = "xAI"
	}

	metadata := MetadataFromTokenStorage(tokenStorage, now)
	metadata[xaiUsingAPIKey] = usingAPI
	attributes := AttributesFromTokenStorage(tokenStorage)
	attributes[xaiUsingAPIKey] = strconv.FormatBool(usingAPI)

	return &coreauth.Auth{
		ID:         fileName,
		Provider:   "xai",
		FileName:   fileName,
		Label:      label,
		Storage:    tokenStorage,
		Metadata:   metadata,
		Attributes: attributes,
	}
}

func MetadataFromTokenStorage(tokenStorage *internalxai.TokenStorage, now time.Time) map[string]any {
	if now.IsZero() {
		now = time.Now()
	}
	metadata := map[string]any{
		"type":      "xai",
		"auth_kind": "oauth",
		"timestamp": now.UnixMilli(),
	}
	if tokenStorage == nil {
		return metadata
	}

	metadata["access_token"] = tokenStorage.AccessToken
	metadata["refresh_token"] = tokenStorage.RefreshToken
	metadata["id_token"] = tokenStorage.IDToken
	metadata["token_type"] = tokenStorage.TokenType
	metadata["expires_in"] = tokenStorage.ExpiresIn
	metadata["expired"] = tokenStorage.Expire
	metadata["last_refresh"] = tokenStorage.LastRefresh
	metadata["base_url"] = firstNonEmpty(tokenStorage.BaseURL, internalxai.DefaultAPIBaseURL)
	metadata["redirect_uri"] = tokenStorage.RedirectURI
	metadata["token_endpoint"] = tokenStorage.TokenEndpoint
	if email := strings.TrimSpace(tokenStorage.Email); email != "" {
		metadata["email"] = email
	}
	if subject := strings.TrimSpace(tokenStorage.Subject); subject != "" {
		metadata["sub"] = subject
	}
	return metadata
}

func AttributesFromTokenStorage(tokenStorage *internalxai.TokenStorage) map[string]string {
	baseURL := internalxai.DefaultAPIBaseURL
	if tokenStorage != nil {
		baseURL = firstNonEmpty(tokenStorage.BaseURL, baseURL)
	}
	return map[string]string{
		"auth_kind": "oauth",
		"base_url":  baseURL,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

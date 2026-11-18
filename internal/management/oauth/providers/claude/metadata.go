package claude

import (
	"fmt"
	"strings"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenStorage(tokenStorage *internalclaude.ClaudeTokenStorage) *coreauth.Auth {
	if tokenStorage == nil {
		return nil
	}
	fileName := CredentialFileName(tokenStorage.Email)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "claude",
		FileName: fileName,
		Storage:  tokenStorage,
		Metadata: MetadataFromTokenStorage(tokenStorage),
	}
}

func MetadataFromTokenStorage(tokenStorage *internalclaude.ClaudeTokenStorage) map[string]any {
	metadata := map[string]any{
		"type": "claude",
	}
	if tokenStorage == nil {
		return metadata
	}
	if email := strings.TrimSpace(tokenStorage.Email); email != "" {
		metadata["email"] = email
	}
	if accessToken := strings.TrimSpace(tokenStorage.AccessToken); accessToken != "" {
		metadata["access_token"] = accessToken
	}
	if refreshToken := strings.TrimSpace(tokenStorage.RefreshToken); refreshToken != "" {
		metadata["refresh_token"] = refreshToken
	}
	if expired := strings.TrimSpace(tokenStorage.Expire); expired != "" {
		metadata["expired"] = expired
	}
	if lastRefresh := strings.TrimSpace(tokenStorage.LastRefresh); lastRefresh != "" {
		metadata["last_refresh"] = lastRefresh
	}
	return metadata
}

func CredentialFileName(email string) string {
	return fmt.Sprintf("claude-%s.json", email)
}

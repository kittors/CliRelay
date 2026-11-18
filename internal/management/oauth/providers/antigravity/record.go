package antigravity

import (
	"strings"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromTokenResponse(tokenResp *internalantigravity.TokenResponse, email, projectID string, now time.Time) *coreauth.Auth {
	if tokenResp == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	email = strings.TrimSpace(email)
	projectID = strings.TrimSpace(projectID)
	fileName := internalantigravity.CredentialFileName(email)
	label := email
	if label == "" {
		label = "antigravity"
	}

	return &coreauth.Auth{
		ID:       fileName,
		Provider: "antigravity",
		FileName: fileName,
		Label:    label,
		Metadata: MetadataFromTokenResponse(tokenResp, email, projectID, now),
	}
}

func MetadataFromTokenResponse(tokenResp *internalantigravity.TokenResponse, email, projectID string, now time.Time) map[string]any {
	if now.IsZero() {
		now = time.Now()
	}
	metadata := map[string]any{
		"type": "antigravity",
	}
	if tokenResp == nil {
		return metadata
	}

	metadata["access_token"] = tokenResp.AccessToken
	metadata["refresh_token"] = tokenResp.RefreshToken
	metadata["expires_in"] = tokenResp.ExpiresIn
	metadata["timestamp"] = now.UnixMilli()
	metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	if trimmedEmail := strings.TrimSpace(email); trimmedEmail != "" {
		metadata["email"] = trimmedEmail
	}
	if trimmedProjectID := strings.TrimSpace(projectID); trimmedProjectID != "" {
		metadata["project_id"] = trimmedProjectID
	}
	return metadata
}

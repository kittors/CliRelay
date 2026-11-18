package kimi

import (
	"fmt"
	"strings"
	"time"

	internalkimi "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kimi"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func RecordFromAuthBundle(tokenStorage *internalkimi.KimiTokenStorage, authBundle *internalkimi.KimiAuthBundle, now time.Time) *coreauth.Auth {
	if tokenStorage == nil || authBundle == nil || authBundle.TokenData == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	fileName := CredentialFileName(now)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "kimi",
		FileName: fileName,
		Label:    "Kimi User",
		Storage:  tokenStorage,
		Metadata: MetadataFromAuthBundle(authBundle, now),
	}
}

func MetadataFromAuthBundle(authBundle *internalkimi.KimiAuthBundle, now time.Time) map[string]any {
	if now.IsZero() {
		now = time.Now()
	}
	metadata := map[string]any{
		"type":      "kimi",
		"timestamp": now.UnixMilli(),
	}
	if authBundle == nil || authBundle.TokenData == nil {
		return metadata
	}

	metadata["access_token"] = authBundle.TokenData.AccessToken
	metadata["refresh_token"] = authBundle.TokenData.RefreshToken
	metadata["token_type"] = authBundle.TokenData.TokenType
	metadata["scope"] = authBundle.TokenData.Scope
	if authBundle.TokenData.ExpiresAt > 0 {
		metadata["expired"] = time.Unix(authBundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if deviceID := strings.TrimSpace(authBundle.DeviceID); deviceID != "" {
		metadata["device_id"] = deviceID
	}
	return metadata
}

func CredentialFileName(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	return fmt.Sprintf("kimi-%d.json", now.UnixMilli())
}

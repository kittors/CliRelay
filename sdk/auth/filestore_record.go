package auth

import (
	"strings"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// NewFileAuthRecord builds the runtime Auth for credential document metadata
// stored under id (its auth-dir relative path) the way FileTokenStore does
// when it loads the file at path. It has no side effects. Stores that keep the
// document somewhere else use it so a credential looks the same to the manager
// whichever backend loaded it, auth_index included (FileName is the ID).
func NewFileAuthRecord(id, path, provider string, metadata map[string]any, modTime time.Time) *cliproxyauth.Auth {
	auth := &cliproxyauth.Auth{
		ID:               id,
		TenantID:         cliproxyauth.TenantIDFromAuthID(id),
		Provider:         provider,
		Prefix:           metadataString(metadata, "prefix"),
		ProxyURL:         metadataString(metadata, "proxy_url", "proxy-url", "proxyUrl"),
		ProxyID:          metadataString(metadata, "proxy_id", "proxy-id", "proxyId"),
		FileName:         id,
		Label:            fileAuthLabel(metadata),
		Status:           cliproxyauth.StatusActive,
		Attributes:       buildFileAuthAttributes(path, metadata),
		Metadata:         metadata,
		CreatedAt:        modTime,
		UpdatedAt:        modTime,
		LastRefreshedAt:  time.Time{},
		NextRefreshAfter: time.Time{},
	}
	cliproxyauth.RestorePersistedDisabled(auth)
	return auth
}

func fileAuthLabel(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if v, ok := metadata["label"].(string); ok && v != "" {
		return v
	}
	if v, ok := metadata["email"].(string); ok && v != "" {
		return v
	}
	if project, ok := metadata["project_id"].(string); ok && project != "" {
		return project
	}
	return ""
}

// buildFileAuthAttributes keeps disk-loaded credentials aligned with OAuth login
// and management upload registration.
func buildFileAuthAttributes(path string, metadata map[string]any) map[string]string {
	attrs := map[string]string{"path": path}
	if email := metadataString(metadata, "email"); email != "" {
		attrs["email"] = email
	}
	if authKind := metadataString(metadata, "auth_kind", "authKind"); authKind != "" {
		attrs["auth_kind"] = authKind
	}
	if baseURL := metadataString(metadata, "base_url", "base-url", "baseUrl"); baseURL != "" {
		attrs["base_url"] = baseURL
	}
	if apiKey := metadataString(metadata, "api_key", "api-key", "apiKey"); apiKey != "" {
		attrs["api_key"] = apiKey
	}
	if raw, ok := metadata["using_api"]; ok {
		switch value := raw.(type) {
		case bool:
			if value {
				attrs["using_api"] = "true"
			} else {
				attrs["using_api"] = "false"
			}
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				attrs["using_api"] = trimmed
			}
		}
	}
	return attrs
}

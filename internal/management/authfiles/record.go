package authfiles

import (
	"path/filepath"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type RecordOptions struct {
	AuthDir  string
	TenantID string
	Path     string
	Provider string
	Metadata map[string]any
	Existing *coreauth.Auth
	Now      time.Time
}

func BuildRecord(opts RecordOptions) *coreauth.Auth {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	// FileName must match FileTokenStore.idFor / readAuthFile so EnsureIndex seeds stay
	// stable across upload-register and disk-reload paths (tenant-relative IDs included).
	authID := AuthIDForPath(opts.AuthDir, path)
	if authID == "" {
		authID = path
	}
	fileName := strings.TrimSpace(authID)
	if fileName == "" {
		fileName = filepath.Base(path)
	}
	lastRefresh, hasLastRefresh := ExtractLastRefreshTimestamp(opts.Metadata)
	disabled := MetadataBool(opts.Metadata, "disabled")
	status := coreauth.StatusActive
	if disabled {
		status = coreauth.StatusDisabled
	}
	auth := &coreauth.Auth{
		ID:         authID,
		TenantID:   NormalizeTenantID(opts.TenantID),
		Provider:   opts.Provider,
		Prefix:     MetadataString(opts.Metadata, "prefix"),
		ProxyURL:   MetadataString(opts.Metadata, "proxy_url", "proxy-url", "proxyUrl"),
		ProxyID:    MetadataString(opts.Metadata, "proxy_id", "proxy-id", "proxyId"),
		FileName:   fileName,
		Label:      ChannelLabelFromMetadata(opts.Metadata, opts.Provider),
		Status:     status,
		Disabled:   disabled,
		Attributes: buildRecordAttributes(path, opts.Metadata),
		Metadata:   opts.Metadata,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if opts.Existing != nil {
		auth.CreatedAt = opts.Existing.CreatedAt
		if !hasLastRefresh {
			auth.LastRefreshedAt = opts.Existing.LastRefreshedAt
		}
		auth.NextRefreshAfter = opts.Existing.NextRefreshAfter
		auth.Runtime = opts.Existing.Runtime
	}
	return auth
}

// buildRecordAttributes mirrors the attribute surface FileTokenStore and OAuth
// login flows persist so imported credentials behave like freshly logged-in ones.
func buildRecordAttributes(path string, metadata map[string]any) map[string]string {
	attrs := map[string]string{
		"path":   path,
		"source": path,
	}
	if email := MetadataString(metadata, "email"); email != "" {
		attrs["email"] = email
	}
	if authKind := MetadataString(metadata, "auth_kind", "authKind"); authKind != "" {
		attrs["auth_kind"] = authKind
	}
	if baseURL := MetadataString(metadata, "base_url", "base-url", "baseUrl"); baseURL != "" {
		attrs["base_url"] = baseURL
	}
	if apiKey := MetadataString(metadata, "api_key", "api-key", "apiKey"); apiKey != "" {
		attrs["api_key"] = apiKey
	}
	if usingAPI, ok := MetadataBoolPresence(metadata, "using_api", "using-api", "usingApi"); ok {
		if usingAPI {
			attrs["using_api"] = "true"
		} else {
			attrs["using_api"] = "false"
		}
	}
	return attrs
}

func MetadataBool(metadata map[string]any, keys ...string) bool {
	value, ok := MetadataBoolPresence(metadata, keys...)
	return ok && value
}

func MetadataBoolPresence(metadata map[string]any, keys ...string) (bool, bool) {
	if len(metadata) == 0 {
		return false, false
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		raw, ok := metadata[key]
		if !ok || raw == nil {
			continue
		}
		switch value := raw.(type) {
		case bool:
			return value, true
		case string:
			trimmed := strings.TrimSpace(strings.ToLower(value))
			if trimmed == "true" || trimmed == "1" || trimmed == "yes" {
				return true, true
			}
			if trimmed == "false" || trimmed == "0" || trimmed == "no" {
				return false, true
			}
		case float64:
			return value != 0, true
		case int:
			return value != 0, true
		case int64:
			return value != 0, true
		}
	}
	return false, false
}

func ChannelLabelFromMetadata(metadata map[string]any, provider string) string {
	if metadata != nil {
		if raw, ok := metadata["label"].(string); ok {
			if label := strings.TrimSpace(raw); label != "" {
				return label
			}
		}
		if raw, ok := metadata["email"].(string); ok {
			if email := strings.TrimSpace(raw); email != "" {
				return email
			}
		}
	}
	return strings.TrimSpace(provider)
}

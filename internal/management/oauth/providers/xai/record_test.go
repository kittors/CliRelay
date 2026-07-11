package xai

import (
	"testing"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
)

func TestRecordFromTokenStorageBuildsPersistableRecord(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 30, 0, 123000000, time.UTC)
	storage := &internalxai.TokenStorage{
		AccessToken:   "access-token",
		RefreshToken:  "refresh-token",
		IDToken:       "id-token",
		TokenType:     "Bearer",
		ExpiresIn:     3600,
		Expire:        now.Add(time.Hour).Format(time.RFC3339),
		LastRefresh:   now.Format(time.RFC3339),
		Email:         " user@example.com ",
		Subject:       "subject-1",
		BaseURL:       "https://api.x.ai/v1",
		RedirectURI:   "http://127.0.0.1:56121/callback",
		TokenEndpoint: "https://auth.x.ai/token",
	}

	record := RecordFromTokenStorage(storage, now, true)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	if record.ID != "xai-user@example.com.json" || record.FileName != "xai-user@example.com.json" {
		t.Fatalf("ID/FileName = %q/%q, want xai-user@example.com.json", record.ID, record.FileName)
	}
	if record.Provider != "xai" || record.Label != "user@example.com" || record.Storage != storage {
		t.Fatalf("provider/label/storage = %q/%q/%#v", record.Provider, record.Label, record.Storage)
	}
	if got := record.Attributes["auth_kind"]; got != "oauth" {
		t.Fatalf("attributes[auth_kind] = %q, want oauth", got)
	}
	if got := record.Attributes["using_api"]; got != "true" {
		t.Fatalf("attributes[using_api] = %q, want true", got)
	}
	if got, ok := record.Metadata["using_api"].(bool); !ok || !got {
		t.Fatalf("metadata[using_api] = %#v, want true", record.Metadata["using_api"])
	}
	for key, want := range map[string]string{
		"type":           "xai",
		"access_token":   "access-token",
		"refresh_token":  "refresh-token",
		"id_token":       "id-token",
		"token_type":     "Bearer",
		"expired":        storage.Expire,
		"last_refresh":   storage.LastRefresh,
		"base_url":       "https://api.x.ai/v1",
		"redirect_uri":   "http://127.0.0.1:56121/callback",
		"token_endpoint": "https://auth.x.ai/token",
		"auth_kind":      "oauth",
		"email":          "user@example.com",
		"sub":            "subject-1",
	} {
		if got, _ := record.Metadata[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	if got, _ := record.Metadata["expires_in"].(int); got != 3600 {
		t.Fatalf("metadata[expires_in] = %#v, want 3600", record.Metadata["expires_in"])
	}
}

func TestRecordFromTokenStorageHandlesNilOrEmptyAccessToken(t *testing.T) {
	if record := RecordFromTokenStorage(nil, time.Time{}, false); record != nil {
		t.Fatalf("RecordFromTokenStorage(nil) = %#v, want nil", record)
	}
	if record := RecordFromTokenStorage(&internalxai.TokenStorage{}, time.Time{}, false); record != nil {
		t.Fatalf("RecordFromTokenStorage(empty token) = %#v, want nil", record)
	}
}

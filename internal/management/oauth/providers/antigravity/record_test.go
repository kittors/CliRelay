package antigravity

import (
	"testing"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
)

func TestRecordFromTokenResponseBuildsPersistableRecord(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 30, 0, 0, time.UTC)
	record := RecordFromTokenResponse(&internalantigravity.TokenResponse{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresIn:    3600,
	}, " user@example.com ", " project-1 ", now)

	if record == nil {
		t.Fatal("RecordFromTokenResponse() = nil")
	}
	if record.ID != "antigravity-user@example.com.json" || record.FileName != "antigravity-user@example.com.json" {
		t.Fatalf("ID/FileName = %q/%q, want antigravity-user@example.com.json", record.ID, record.FileName)
	}
	if record.Provider != "antigravity" || record.Label != "user@example.com" || record.Storage != nil {
		t.Fatalf("provider/label/storage = %q/%q/%#v", record.Provider, record.Label, record.Storage)
	}

	wantExpired := now.Add(time.Hour).Format(time.RFC3339)
	for key, want := range map[string]string{
		"type":          "antigravity",
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"email":         "user@example.com",
		"project_id":    "project-1",
		"expired":       wantExpired,
	} {
		if got, _ := record.Metadata[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	if got, _ := record.Metadata["expires_in"].(int64); got != 3600 {
		t.Fatalf("metadata[expires_in] = %#v, want 3600", record.Metadata["expires_in"])
	}
	if got, _ := record.Metadata["timestamp"].(int64); got != now.UnixMilli() {
		t.Fatalf("metadata[timestamp] = %#v, want %d", record.Metadata["timestamp"], now.UnixMilli())
	}
}

func TestRecordFromTokenResponseUsesProviderFallbacks(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 30, 0, 0, time.UTC)
	record := RecordFromTokenResponse(&internalantigravity.TokenResponse{}, "", "", now)

	if record == nil {
		t.Fatal("RecordFromTokenResponse() = nil")
	}
	if record.ID != "antigravity.json" || record.FileName != "antigravity.json" || record.Label != "antigravity" {
		t.Fatalf("fallback record = %#v", record)
	}
	if _, ok := record.Metadata["email"]; ok {
		t.Fatalf("metadata[email] = %#v, want omitted", record.Metadata["email"])
	}
	if _, ok := record.Metadata["project_id"]; ok {
		t.Fatalf("metadata[project_id] = %#v, want omitted", record.Metadata["project_id"])
	}
}

func TestRecordFromTokenResponseHandlesNilResponse(t *testing.T) {
	if record := RecordFromTokenResponse(nil, "user@example.com", "project-1", time.Time{}); record != nil {
		t.Fatalf("RecordFromTokenResponse(nil) = %#v, want nil", record)
	}
}

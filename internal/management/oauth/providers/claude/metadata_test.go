package claude

import (
	"testing"
	"time"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
)

func TestRecordFromTokenStorageBuildsPersistableRecord(t *testing.T) {
	storage := &internalclaude.ClaudeTokenStorage{
		AccessToken: "access-token",
		Email:       "claude@example.com",
	}

	record := RecordFromTokenStorage(storage)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	if record.ID != "claude-claude@example.com.json" || record.FileName != "claude-claude@example.com.json" {
		t.Fatalf("ID/FileName = %q/%q, want claude-claude@example.com.json", record.ID, record.FileName)
	}
	if record.Provider != "claude" || record.Storage != storage {
		t.Fatalf("provider/storage = %q/%#v, want claude/original storage", record.Provider, record.Storage)
	}
	if got, _ := record.Metadata["email"].(string); got != "claude@example.com" {
		t.Fatalf("metadata[email] = %q, want claude@example.com", got)
	}
}

func TestMetadataFromTokenStorageIncludesRuntimeTokens(t *testing.T) {
	expired := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	lastRefresh := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)

	meta := MetadataFromTokenStorage(&internalclaude.ClaudeTokenStorage{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Email:        "claude@example.com",
		Expire:       expired,
		LastRefresh:  lastRefresh,
	})

	for key, want := range map[string]string{
		"type":          "claude",
		"access_token":  "access-token",
		"refresh_token": "refresh-token",
		"email":         "claude@example.com",
		"expired":       expired,
		"last_refresh":  lastRefresh,
	} {
		if got, _ := meta[key].(string); got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestMetadataFromTokenStorageHandlesNilStorage(t *testing.T) {
	meta := MetadataFromTokenStorage(nil)
	if got, _ := meta["type"].(string); got != "claude" {
		t.Fatalf("metadata[type] = %q, want claude", got)
	}
	if len(meta) != 1 {
		t.Fatalf("metadata = %#v, want only type", meta)
	}
}

func TestRecordFromTokenStorageHandlesNilStorage(t *testing.T) {
	if record := RecordFromTokenStorage(nil); record != nil {
		t.Fatalf("RecordFromTokenStorage(nil) = %#v, want nil", record)
	}
}

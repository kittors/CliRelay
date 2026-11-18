package qwen

import (
	"testing"
	"time"

	internalqwen "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/qwen"
)

func TestRecordFromTokenStorageBuildsTimestampedRecord(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 30, 0, 123000000, time.UTC)
	storage := &internalqwen.QwenTokenStorage{
		AccessToken: "access-token",
		Email:       "existing@example.com",
	}

	record := RecordFromTokenStorage(storage, now)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}

	identifier := "1780749000123"
	if storage.Email != identifier {
		t.Fatalf("storage.Email = %q, want %q", storage.Email, identifier)
	}
	if record.ID != "qwen-"+identifier+".json" || record.FileName != "qwen-"+identifier+".json" {
		t.Fatalf("ID/FileName = %q/%q, want timestamped qwen filename", record.ID, record.FileName)
	}
	if record.Provider != "qwen" || record.Storage != storage {
		t.Fatalf("provider/storage = %q/%#v, want qwen/original storage", record.Provider, record.Storage)
	}
	if got, _ := record.Metadata["email"].(string); got != identifier {
		t.Fatalf("metadata[email] = %q, want %q", got, identifier)
	}
}

func TestRecordFromTokenStorageHandlesNilStorage(t *testing.T) {
	if record := RecordFromTokenStorage(nil, time.Time{}); record != nil {
		t.Fatalf("RecordFromTokenStorage(nil) = %#v, want nil", record)
	}
}

func TestCredentialFileNameTrimsIdentifier(t *testing.T) {
	if got := CredentialFileName(" 123 "); got != "qwen-123.json" {
		t.Fatalf("CredentialFileName() = %q, want qwen-123.json", got)
	}
}

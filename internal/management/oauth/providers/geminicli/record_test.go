package geminicli

import (
	"testing"

	geminiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gemini"
)

func TestRecordFromTokenStorageBuildsPersistableRecord(t *testing.T) {
	storage := &geminiauth.GeminiTokenStorage{
		Email:     "gemini@example.com",
		ProjectID: "project-1",
		Auto:      true,
		Checked:   true,
	}

	record := RecordFromTokenStorage(storage)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	if record.ID != "gemini-gemini@example.com-project-1.json" || record.FileName != "gemini-gemini@example.com-project-1.json" {
		t.Fatalf("ID/FileName = %q/%q, want gemini-gemini@example.com-project-1.json", record.ID, record.FileName)
	}
	if record.Provider != "gemini" || record.Storage != storage {
		t.Fatalf("provider/storage = %q/%#v, want gemini/original storage", record.Provider, record.Storage)
	}
	if got, _ := record.Metadata["project_id"].(string); got != "project-1" {
		t.Fatalf("metadata[project_id] = %q, want project-1", got)
	}
	if got, _ := record.Metadata["checked"].(bool); !got {
		t.Fatalf("metadata[checked] = %v, want true", got)
	}
}

func TestRecordFromTokenStorageBuildsAllProjectsFileName(t *testing.T) {
	storage := &geminiauth.GeminiTokenStorage{
		Email:     "gemini@example.com",
		ProjectID: "project-1,project-2",
	}

	record := RecordFromTokenStorage(storage)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	if record.FileName != "gemini-gemini@example.com-all.json" {
		t.Fatalf("FileName = %q, want gemini-gemini@example.com-all.json", record.FileName)
	}
}

func TestRecordFromTokenStorageHandlesNilStorage(t *testing.T) {
	if record := RecordFromTokenStorage(nil); record != nil {
		t.Fatalf("RecordFromTokenStorage(nil) = %#v, want nil", record)
	}
}

func TestMetadataFromTokenStorageHandlesNilStorage(t *testing.T) {
	metadata := MetadataFromTokenStorage(nil)
	if len(metadata) != 0 {
		t.Fatalf("MetadataFromTokenStorage(nil) = %#v, want empty map", metadata)
	}
}

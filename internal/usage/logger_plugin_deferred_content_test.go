package usage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestLoggerPluginPersistsDeferredContentFiles(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	registerTestAPIKey(t, "deferred-key-id", "sk-deferred", "Deferred")

	var deferredPaths []string
	writeTemp := func(pattern, content string) string {
		t.Helper()
		file, err := os.CreateTemp("", pattern)
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		if _, err = file.WriteString(content); err != nil {
			t.Fatalf("write temp content: %v", err)
		}
		if err = file.Close(); err != nil {
			t.Fatalf("close temp content: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(file.Name()) })
		deferredPaths = append(deferredPaths, file.Name())
		return file.Name()
	}

	input := `{"model":"gpt-test","stream":true}`
	output := "data: {\"id\":\"resp-test\"}\n\n"
	detail := `{"response":{"upstream_log":"ok"}}`
	plugin := NewLoggerPlugin()
	plugin.HandleUsage(context.Background(), coreusage.Record{
		APIKey:            "sk-deferred",
		Model:             "gpt-test",
		RequestedAt:       time.Now().UTC(),
		Detail:            coreusage.Detail{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		InputContentPath:  writeTemp("usage-input-*", input),
		OutputContentPath: writeTemp("usage-output-*", output),
		DetailContentPath: writeTemp("usage-detail-*", detail),
	})

	for _, path := range deferredPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("consumed deferred content file still exists at %s: %v", path, err)
		}
	}

	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1, APIKeyIDs: []string{"deferred-key-id"}})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(logs.Items) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs.Items))
	}
	content, err := QueryLogContent(logs.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent: %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("persisted deferred input/output mismatch: %#v", content)
	}
	detailPart, err := QueryLogContentPart(logs.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details): %v", err)
	}
	if detailPart.Content != detail {
		t.Fatalf("persisted deferred detail = %q, want %q", detailPart.Content, detail)
	}
}

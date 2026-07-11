package usage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestPurgeStoredRequestBodiesSanitizesDetailsAndReclaimsStorage(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	details := `{"client":{"ip":"203.0.113.8"},"upstream":{"request_log":"=== API REQUEST 1 ===\nHeaders:\nX-Test: yes\nBody:\nsecret-request"},"response":{"upstream_log":"=== API RESPONSE 1 ===\nStatus: 200\nBody:\nsecret-response"}}`
	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, `{"secret":"request"}`, `{"secret":"response"}`, details)

	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil || len(logs.Items) != 1 {
		t.Fatalf("QueryLogs() result=%+v error=%v", logs, err)
	}
	SetRequestLogBodyStorageEnabled(false)
	result, err := PurgeStoredRequestBodies()
	if err != nil {
		t.Fatalf("PurgeStoredRequestBodies() error = %v", err)
	}
	if result.ClearedBodyRows != 1 || result.SanitizedDetailRows != 1 || !result.ReclaimedStorage {
		t.Fatalf("unexpected purge result: %+v", result)
	}

	input, err := QueryLogContentPart(logs.Items[0].ID, "input")
	if err != nil || input.Content != "" {
		t.Fatalf("input content=%q error=%v, want empty", input.Content, err)
	}
	detail, err := QueryLogContentPart(logs.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) error = %v", err)
	}
	if !strings.Contains(detail.Content, "203.0.113.8") || !strings.Contains(detail.Content, "X-Test: yes") {
		t.Fatalf("detail metadata was not preserved: %q", detail.Content)
	}
	if strings.Contains(detail.Content, "secret-request") || strings.Contains(detail.Content, "secret-response") {
		t.Fatalf("detail bodies were not removed: %q", detail.Content)
	}
}

func TestPurgeStoredRequestBodiesDropsMalformedHistoricalDetails(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "input", "output", "malformed detail with possible body")
	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil || len(logs.Items) != 1 {
		t.Fatalf("QueryLogs() result=%+v error=%v", logs, err)
	}

	SetRequestLogBodyStorageEnabled(false)
	if _, err = PurgeStoredRequestBodies(); err != nil {
		t.Fatalf("PurgeStoredRequestBodies() error = %v", err)
	}
	detail, err := QueryLogContentPart(logs.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) error = %v", err)
	}
	if detail.Content != "" {
		t.Fatalf("malformed detail content = %q, want empty", detail.Content)
	}
}

func TestDisabledBodyStorageRepairsHistoricalDetailsOnStartup(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	enabled := config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}
	if err := InitDB(dbPath, enabled, time.UTC); err != nil {
		t.Fatalf("InitDB(enabled): %v", err)
	}
	details := `{"client":{"ip":"203.0.113.8"},"upstream":{"request_log":"=== API REQUEST 1 ===\nBody:\nsecret-request"}}`
	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "input", "output", details)
	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil || len(logs.Items) != 1 {
		t.Fatalf("QueryLogs() result=%+v error=%v", logs, err)
	}
	logID := logs.Items[0].ID
	CloseDB()

	disabled := enabled
	disabled.StoreContent = false
	if err = InitDB(dbPath, disabled, time.UTC); err != nil {
		t.Fatalf("InitDB(disabled): %v", err)
	}
	t.Cleanup(CloseDB)

	deadline := time.Now().Add(5 * time.Second)
	for {
		input, inputErr := QueryLogContentPart(logID, "input")
		detail, detailErr := QueryLogContentPart(logID, "details")
		if inputErr == nil && detailErr == nil && input.Content == "" && !strings.Contains(detail.Content, "secret-request") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup repair timed out: input=%q inputErr=%v detail=%q detailErr=%v", input.Content, inputErr, detail.Content, detailErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package usage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func initTestUsageDB(t *testing.T, cfg config.RequestLogStorageConfig) {
	t.Helper()
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, cfg); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
}

func TestInsertLogStoresCompressedContentOutsideMainTable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello world"}]}`
	output := `{"id":"resp_123","output":"done"}`

	InsertLog("sk-test", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, TokenStats{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
	}, input, output)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}
	if !result.Items[0].HasContent {
		t.Fatalf("expected HasContent to be true")
	}

	content, err := QueryLogContent(result.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input {
		t.Fatalf("InputContent = %q, want %q", content.InputContent, input)
	}
	if content.OutputContent != output {
		t.Fatalf("OutputContent = %q, want %q", content.OutputContent, output)
	}

	db := getDB()
	var legacyInput, legacyOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		result.Items[0].ID,
	).Scan(&legacyInput, &legacyOutput); err != nil {
		t.Fatalf("query legacy columns: %v", err)
	}
	if legacyInput != "" || legacyOutput != "" {
		t.Fatalf("expected main table content columns to be empty, got input=%q output=%q", legacyInput, legacyOutput)
	}

	var compressedInput, compressedOutput []byte
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_log_content WHERE log_id = ?",
		result.Items[0].ID,
	).Scan(&compressedInput, &compressedOutput); err != nil {
		t.Fatalf("query compressed content row: %v", err)
	}
	if len(compressedInput) == 0 || len(compressedOutput) == 0 {
		t.Fatalf("expected compressed content blobs to be present")
	}
}

func TestMigrateLegacyContentBatchMovesContentOutOfMainTable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	db := getDB()
	timestamp := time.Now().UTC()
	input := "legacy-input"
	output := "legacy-output"

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, input, output,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	migrated, err := migrateLegacyContentBatch(db, 100)
	if err != nil {
		t.Fatalf("migrateLegacyContentBatch() error = %v", err)
	}
	if migrated != 1 {
		t.Fatalf("migrated = %d, want 1", migrated)
	}

	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("unexpected migrated content: %+v", content)
	}

	var legacyInput, legacyOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		logID,
	).Scan(&legacyInput, &legacyOutput); err != nil {
		t.Fatalf("query cleared legacy columns: %v", err)
	}
	if legacyInput != "" || legacyOutput != "" {
		t.Fatalf("expected legacy columns cleared after migration, got input=%q output=%q", legacyInput, legacyOutput)
	}
}

func TestCleanupExpiredLogContentKeepsMetadataRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC().AddDate(0, 0, -40)
	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-old", "old-model", "source", "channel", "auth-old",
		0, 5, 1, 1, 0, 0, 2, 0,
	)
	if err != nil {
		t.Fatalf("insert metadata row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := insertLogContentTx(tx, logID, timestamp, "expired-input", "expired-output"); err != nil {
		t.Fatalf("insertLogContentTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	_, err = cleanupExpiredLogContent(db)
	if err != nil {
		t.Fatalf("cleanupExpiredLogContent() error = %v", err)
	}

	var metadataRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE id = ?", logID).Scan(&metadataRows); err != nil {
		t.Fatalf("count metadata rows: %v", err)
	}
	if metadataRows != 1 {
		t.Fatalf("metadata row count = %d, want 1", metadataRows)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}
}

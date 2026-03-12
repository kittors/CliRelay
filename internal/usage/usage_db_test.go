package usage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func makePseudoRandomText(size int) string {
	b := make([]byte, size)
	var x uint32 = 1
	for i := range b {
		x = 1664525*x + 1013904223
		b[i] = byte(32 + x%95)
	}
	return string(b)
}

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

func TestGetRequestLogStorageBytesCountsCompressedAndLegacyContent(t *testing.T) {
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

	db := getDB()
	var compressedInputBytes, compressedOutputBytes int64
	if err := db.QueryRow(
		`SELECT length(input_content), length(output_content)
		 FROM request_log_content
		 ORDER BY log_id DESC
		 LIMIT 1`,
	).Scan(&compressedInputBytes, &compressedOutputBytes); err != nil {
		t.Fatalf("query compressed content lengths: %v", err)
	}

	legacyInput := "legacy-inline-input"
	legacyOutput := "legacy-inline-output"
	if _, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, legacyInput, legacyOutput,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	totalBytes, err := GetRequestLogStorageBytes()
	if err != nil {
		t.Fatalf("GetRequestLogStorageBytes() error = %v", err)
	}

	want := compressedInputBytes + compressedOutputBytes + int64(len(legacyInput)+len(legacyOutput))
	if totalBytes != want {
		t.Fatalf("GetRequestLogStorageBytes() = %d, want %d", totalBytes, want)
	}
}

func TestMigrateLegacyContentBatchKeepsAllContentWhenRetentionUnlimited(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   0,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC().AddDate(0, 0, -90)
	input := "legacy-unlimited-input"
	output := "legacy-unlimited-output"

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
}

func TestMigrateLegacyContentBatchPreservesInlineContentWhenStorageDisabled(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           false,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	timestamp := time.Now().UTC()
	input := "legacy-inline-input"
	output := "legacy-inline-output"

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		timestamp.Format(time.RFC3339Nano),
		"sk-inline", "inline-model", "inline-source", "inline-channel", "auth-inline",
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
	if migrated != 0 {
		t.Fatalf("migrated = %d, want 0", migrated)
	}

	var inlineInput, inlineOutput string
	if err := db.QueryRow(
		"SELECT input_content, output_content FROM request_logs WHERE id = ?",
		logID,
	).Scan(&inlineInput, &inlineOutput); err != nil {
		t.Fatalf("query legacy inline content: %v", err)
	}
	if inlineInput != input || inlineOutput != output {
		t.Fatalf("legacy inline content changed: input=%q output=%q", inlineInput, inlineOutput)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}

	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input || content.OutputContent != output {
		t.Fatalf("unexpected fallback content: %+v", content)
	}
}

func TestCleanupExpiredLogContentSkipsWhenStorageDisabledOrRetentionUnlimited(t *testing.T) {
	testCases := []struct {
		name string
		cfg  config.RequestLogStorageConfig
	}{
		{
			name: "storage disabled",
			cfg: config.RequestLogStorageConfig{
				StoreContent:           false,
				ContentRetentionDays:   30,
				CleanupIntervalMinutes: 1440,
				VacuumOnCleanup:        false,
			},
		},
		{
			name: "retention unlimited",
			cfg: config.RequestLogStorageConfig{
				StoreContent:           true,
				ContentRetentionDays:   0,
				CleanupIntervalMinutes: 1440,
				VacuumOnCleanup:        false,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			initTestUsageDB(t, tc.cfg)

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

			inputCompressed, err := compressLogContent("expired-input")
			if err != nil {
				t.Fatalf("compressLogContent(input) error = %v", err)
			}
			outputCompressed, err := compressLogContent("expired-output")
			if err != nil {
				t.Fatalf("compressLogContent(output) error = %v", err)
			}
			if _, err := db.Exec(
				`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
				 VALUES (?, ?, ?, ?, ?)`,
				logID,
				timestamp.Format(time.RFC3339Nano),
				requestLogContentCompression,
				inputCompressed,
				outputCompressed,
			); err != nil {
				t.Fatalf("insert request_log_content row: %v", err)
			}

			deleted, err := cleanupExpiredLogContent(db)
			if err != nil {
				t.Fatalf("cleanupExpiredLogContent() error = %v", err)
			}
			if deleted != 0 {
				t.Fatalf("deleted = %d, want 0", deleted)
			}

			var contentRows int
			if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
				t.Fatalf("count content rows: %v", err)
			}
			if contentRows != 1 {
				t.Fatalf("content row count = %d, want 1", contentRows)
			}
		})
	}
}

func TestCleanupOversizedLogContentPrunesOldestRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	maxBytes := int64(1024 * 1024)
	payload := makePseudoRandomText(420 * 1024)
	compressed, err := compressLogContent(payload)
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	rowBytes := int64(len(compressed))
	if rowBytes >= maxBytes {
		t.Fatalf("test payload compressed too large: %d", rowBytes)
	}
	if rowBytes*3 <= maxBytes {
		t.Fatalf("test payload compressed too small to exceed cap: %d", rowBytes)
	}

	insertRawContentRow := func(ts time.Time, apiKey string) int64 {
		t.Helper()
		result, err := db.Exec(
			`INSERT INTO request_logs
				(timestamp, api_key, model, source, channel_name, auth_index,
				 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ts.Format(time.RFC3339Nano),
			apiKey, "model", "source", "channel", apiKey,
			0, 5, 1, 1, 0, 0, 2, 0,
		)
		if err != nil {
			t.Fatalf("insert request_logs row: %v", err)
		}
		logID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId() error = %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
			 VALUES (?, ?, ?, ?, ?)`,
			logID,
			ts.Format(time.RFC3339Nano),
			requestLogContentCompression,
			compressed,
			[]byte{},
		); err != nil {
			t.Fatalf("insert request_log_content row: %v", err)
		}
		return logID
	}

	oldestID := insertRawContentRow(time.Now().UTC().Add(-3*time.Hour), "sk-oldest")
	_ = insertRawContentRow(time.Now().UTC().Add(-2*time.Hour), "sk-middle")
	newestID := insertRawContentRow(time.Now().UTC().Add(-1*time.Hour), "sk-newest")

	deleted, err := cleanupOversizedLogContent(db, maxBytes)
	if err != nil {
		t.Fatalf("cleanupOversizedLogContent() error = %v", err)
	}
	if deleted == 0 {
		t.Fatalf("expected oversized cleanup to delete at least one row")
	}

	totalBytes, err := queryStoredContentBytes(db)
	if err != nil {
		t.Fatalf("queryStoredContentBytes() error = %v", err)
	}
	if totalBytes > maxBytes {
		t.Fatalf("total stored bytes = %d, want <= %d", totalBytes, maxBytes)
	}

	var oldestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", oldestID).Scan(&oldestRows); err != nil {
		t.Fatalf("count oldest row: %v", err)
	}
	if oldestRows != 0 {
		t.Fatalf("expected oldest row to be pruned, count=%d", oldestRows)
	}

	var newestRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", newestID).Scan(&newestRows); err != nil {
		t.Fatalf("count newest row: %v", err)
	}
	if newestRows != 1 {
		t.Fatalf("expected newest row to remain, count=%d", newestRows)
	}
}

func TestInsertLogContentTxSkipsSingleRowLargerThanSizeCap(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
		VacuumOnCleanup:        false,
	})

	db := getDB()
	maxBytes := int64(1024 * 1024)
	payload := makePseudoRandomText(2 * 1024 * 1024)
	compressed, err := compressLogContent(payload)
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	if int64(len(compressed)) <= maxBytes {
		t.Fatalf("expected compressed payload to exceed cap, got %d", len(compressed))
	}

	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"sk-large", "model", "source", "channel", "auth-large",
		0, 5, 1, 1, 0, 0, 2, 0,
	)
	if err != nil {
		t.Fatalf("insert request_logs row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := insertLogContentTx(tx, logID, time.Now().UTC(), payload, ""); err != nil {
		t.Fatalf("insertLogContentTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	var contentRows int
	if err := db.QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content row count = %d, want 0", contentRows)
	}
}

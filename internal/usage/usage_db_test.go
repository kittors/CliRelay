package usage

import (
	"database/sql"
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
	if err := InitDB(dbPath, cfg, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
}

func TestCutoffStartUTCAtUsesProjectTimezoneForDayBoundaries(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	loc := time.FixedZone("UTC+8", 8*3600)
	if err := InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	nowUTC := time.Date(2026, 3, 12, 1, 0, 0, 0, time.UTC) // 09:00 at UTC+8 (local date: 2026-03-12)

	got := cutoffStartUTCAt(nowUTC, 1)
	want := time.Date(2026, 3, 11, 16, 0, 0, 0, time.UTC) // local 2026-03-12 00:00 at UTC+8
	if !got.Equal(want) {
		t.Fatalf("cutoffStartUTCAt(days=1) = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	got = cutoffStartUTCAt(nowUTC, 2)
	want = time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC) // local 2026-03-11 00:00 at UTC+8
	if !got.Equal(want) {
		t.Fatalf("cutoffStartUTCAt(days=2) = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestQueryDailyCallsByAuthIndexesBucketsByProjectTimezone(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	loc := time.FixedZone("UTC+14", 14*3600)
	if err := InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, loc); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	nowLocal := time.Now().In(loc)
	localToday := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 30, 0, 0, loc)
	InsertLog("", "", "gpt-5.4", "codex", "Codex", "auth-local-day", false, localToday, 1, 1, TokenStats{TotalTokens: 1}, "", "")

	points, err := QueryDailyCallsByAuthIndexes([]string{"auth-local-day"}, 1)
	if err != nil {
		t.Fatalf("QueryDailyCallsByAuthIndexes() error = %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points len = %d, want 1: %+v", len(points), points)
	}
	wantDate := localToday.Format("2006-01-02")
	if points[0].Date != wantDate {
		t.Fatalf("point date = %q, want local day %q", points[0].Date, wantDate)
	}
	if points[0].Requests != 1 {
		t.Fatalf("point requests = %d, want 1", points[0].Requests)
	}
}

func TestQuotaSnapshotPointsKeepFineGrainedSeries(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	recordedAt := time.Date(2026, 4, 30, 16, 0, 0, 0, time.UTC)
	resetAt := recordedAt.Add(5 * time.Hour)
	remaining := 100.0

	err := RecordQuotaSnapshotPoints("auth-1", "codex", []QuotaSnapshotPoint{
		{
			RecordedAt:    recordedAt,
			QuotaKey:      "additional:codex_bengalfox:5h",
			QuotaLabel:    "GPT-5.3-Codex-Spark: 5h",
			Percent:       &remaining,
			ResetAt:       &resetAt,
			WindowSeconds: 18000,
		},
	})
	if err != nil {
		t.Fatalf("RecordQuotaSnapshotPoints() error = %v", err)
	}

	points, err := QueryQuotaSnapshotPoints("auth-1", recordedAt.Add(-time.Minute), recordedAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("QueryQuotaSnapshotPoints() error = %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %d, want 1", len(points))
	}
	point := points[0]
	if point.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", point.Provider)
	}
	if point.QuotaKey != "additional:codex_bengalfox:5h" {
		t.Fatalf("QuotaKey = %q", point.QuotaKey)
	}
	if point.QuotaLabel != "GPT-5.3-Codex-Spark: 5h" {
		t.Fatalf("QuotaLabel = %q", point.QuotaLabel)
	}
	if point.Percent == nil || *point.Percent != 100 {
		t.Fatalf("Percent = %v, want 100", point.Percent)
	}
	if point.ResetAt == nil || !point.ResetAt.Equal(resetAt) {
		t.Fatalf("ResetAt = %v, want %v", point.ResetAt, resetAt)
	}
	if point.WindowSeconds != 18000 {
		t.Fatalf("WindowSeconds = %d, want 18000", point.WindowSeconds)
	}
}

func TestQueryLogsSupportsSystemRequestLogFilterValue(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	now := time.Now().UTC()
	InsertLog("POST /image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-1", false, now, 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")
	InsertLog("/v0/management/image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-2", true, now, 120, 12, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")
	InsertLog("sk-live-123", "Primary", "gpt-5.4", "codex", "Codex", "auth-3", false, now, 140, 14, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "")

	result, err := QueryLogs(LogQueryParams{
		Page:   1,
		Size:   10,
		Days:   1,
		APIKey: systemRequestLogFilterValue,
	})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("system filter items = %d, want 2", len(result.Items))
	}
	for _, item := range result.Items {
		if item.APIKey == "sk-live-123" {
			t.Fatalf("unexpected non-system api key in system filter result: %q", item.APIKey)
		}
	}
}

func TestQueryLogContentKeepsMissingFailedOutputEmpty(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	input := `{"model":"gpt-image-2","prompt":"draw a fox"}`
	InsertLog("POST /image-generation/test", "", "gpt-image-2", "codex", "Codex", "auth-1", true, now, 100, 10, TokenStats{}, input, "")

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}

	content, err := QueryLogContent(result.Items[0].ID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	if content.InputContent != input {
		t.Fatalf("InputContent = %q, want %q", content.InputContent, input)
	}
	if content.OutputContent != "" {
		t.Fatalf("OutputContent = %q, want empty historical missing output", content.OutputContent)
	}

	part, err := QueryLogContentPart(result.Items[0].ID, "output")
	if err != nil {
		t.Fatalf("QueryLogContentPart() error = %v", err)
	}
	if part.Content != "" {
		t.Fatalf("part.Content = %q, want empty historical missing output", part.Content)
	}
}

func TestQueryLogContentPartReturnsStoredRequestDetails(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	details := `{"client":{"ip":"203.0.113.8","headers":{"Authorization":"Bearer sk-client-plaintext"}},"upstream":{"headers":{"Authorization":"Bearer sk-upstream-plaintext"}},"response":{"headers":{"X-Request-Id":"req-plaintext"}}}`
	InsertLogWithDetails("sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1", false, now, 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, `{"messages":[]}`, `{"choices":[]}`, details)

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(result.Items))
	}

	part, err := QueryLogContentPart(result.Items[0].ID, "details")
	if err != nil {
		t.Fatalf("QueryLogContentPart(details) error = %v", err)
	}
	if part.Part != "details" {
		t.Fatalf("part.Part = %q, want details", part.Part)
	}
	if part.Content != details {
		t.Fatalf("details content = %q, want %q", part.Content, details)
	}
}

func TestInitDBMigratesFirstTokenColumn(t *testing.T) {
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")

	legacyDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE request_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp DATETIME NOT NULL,
			api_key TEXT NOT NULL DEFAULT '',
			api_key_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			channel_name TEXT NOT NULL DEFAULT '',
			auth_index TEXT NOT NULL DEFAULT '',
			failed INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0,
			input_content TEXT NOT NULL DEFAULT '',
			output_content TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		t.Fatalf("create legacy request_logs table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)

	db := getDB()
	var found bool
	rows, err := db.Query("PRAGMA table_info(request_logs)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(request_logs): %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table info: %v", err)
		}
		if name == "first_token_ms" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected first_token_ms column to exist after InitDB migration")
	}
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

	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 45, TokenStats{
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
	if result.Items[0].FirstTokenMs != 45 {
		t.Fatalf("FirstTokenMs = %d, want %d", result.Items[0].FirstTokenMs, 45)
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
	if err := insertLogContentTx(tx, logID, timestamp, "expired-input", "expired-output", ""); err != nil {
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

	InsertLog("sk-test", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 123, 33, TokenStats{
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
	if err := insertLogContentTx(tx, logID, time.Now().UTC(), payload, "", ""); err != nil {
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

func TestDeleteLogsByAPIKeyRemovesLogsAndContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	timestamp := time.Now().UTC()
	input := `{"messages":[{"role":"user","content":"hello"}]}`
	output := `{"id":"resp_1","output":"done"}`

	// Insert 3 logs: 2 for "sk-target", 1 for "sk-other"
	InsertLog("sk-target", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 100, 10, TokenStats{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	}, input, output)
	InsertLog("sk-target", "", "gpt-test", "source", "channel", "auth-1", false, timestamp, 200, 20, TokenStats{
		InputTokens: 15, OutputTokens: 25, TotalTokens: 40,
	}, input, output)
	InsertLog("sk-other", "", "gpt-test", "source", "channel", "auth-2", false, timestamp, 300, 30, TokenStats{
		InputTokens: 5, OutputTokens: 10, TotalTokens: 15,
	}, input, output)

	// Verify all inserted
	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 3 {
		t.Fatalf("expected 3 log rows, got %d", len(result.Items))
	}

	// Delete logs for sk-target
	deleted, err := DeleteLogsByAPIKey("sk-target")
	if err != nil {
		t.Fatalf("DeleteLogsByAPIKey() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteLogsByAPIKey() deleted = %d, want 2", deleted)
	}

	// Verify only sk-other remains
	result, err = QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs() after delete error = %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("expected 1 log row after delete, got %d", len(result.Items))
	}
	if result.Items[0].APIKey != "sk-other" {
		t.Fatalf("remaining log api_key = %q, want sk-other", result.Items[0].APIKey)
	}

	// Verify content rows are also deleted for sk-target
	db := getDB()
	var contentCount int
	err = db.QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&contentCount)
	if err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	// Only sk-other's content should remain (1 row)
	if contentCount != 1 {
		t.Fatalf("expected 1 content row (sk-other only), got %d", contentCount)
	}
}

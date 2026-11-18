package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// insertContentRowForTrim seeds one request_logs row and its stored content so
// the size-cap trimmer has something to evict.
func insertContentRowForTrim(t *testing.T, ts time.Time, apiKey string, failed int, compressed []byte) int64 {
	t.Helper()
	db := getDB()
	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.Format(time.RFC3339Nano),
		apiKey, "model", "source", "channel", apiKey,
		failed, 5, 1, 1, 0, 0, 2, 0,
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

func contentRowExists(t *testing.T, logID int64) bool {
	t.Helper()
	var rows int
	if err := getDB().QueryRow("SELECT COUNT(*) FROM request_log_content WHERE log_id = ?", logID).Scan(&rows); err != nil {
		t.Fatalf("count content row %d: %v", logID, err)
	}
	return rows > 0
}

// A failed request stores a few hundred bytes of upstream error; a successful
// one stores bodies that run into hundreds of kilobytes. Plain FIFO eviction
// spent the whole budget on successes and took the diagnostics with them — in
// production a 3-day retention setting held under an hour of rows, so the error
// that explained an incident was gone before anyone opened the log.
//
// All three rows here are the same size, so only the eviction order can decide
// which survives.
func TestCleanupOversizedLogContentKeepsFailureDiagnostics(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
	})

	maxBytes := int64(1024 * 1024)
	compressed, err := compressLogContent(makePseudoRandomText(420 * 1024))
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}
	if rowBytes := int64(len(compressed)); rowBytes >= maxBytes || rowBytes*3 <= maxBytes {
		t.Fatalf("payload sized wrong for a three-row cap test: %d", rowBytes)
	}

	now := time.Now().UTC()
	failedID := insertContentRowForTrim(t, now.Add(-3*time.Hour), "sk-failed", 1, compressed)
	oldestOKID := insertContentRowForTrim(t, now.Add(-2*time.Hour), "sk-ok-old", 0, compressed)
	newestOKID := insertContentRowForTrim(t, now.Add(-1*time.Hour), "sk-ok-new", 0, compressed)

	if _, err := cleanupOversizedLogContent(getDB(), maxBytes); err != nil {
		t.Fatalf("cleanupOversizedLogContent() error = %v", err)
	}

	totalBytes, err := queryStoredContentBytes(getDB())
	if err != nil {
		t.Fatalf("queryStoredContentBytes() error = %v", err)
	}
	if totalBytes > maxBytes {
		t.Fatalf("total stored bytes = %d, want <= %d", totalBytes, maxBytes)
	}

	if !contentRowExists(t, failedID) {
		t.Fatal("the oldest row is a failure diagnostic and must outlive successful bodies")
	}
	if contentRowExists(t, oldestOKID) {
		t.Fatal("the oldest successful body should have been evicted first")
	}
	if !contentRowExists(t, newestOKID) {
		t.Fatal("evicting more than the cap required")
	}
}

// Preserving diagnostics must not turn the cap into a suggestion: once nothing
// but failures is left, they are evicted like anything else.
func TestCleanupOversizedLogContentStillEnforcesCapWhenOnlyFailuresRemain(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         1,
	})

	maxBytes := int64(1024 * 1024)
	compressed, err := compressLogContent(makePseudoRandomText(420 * 1024))
	if err != nil {
		t.Fatalf("compressLogContent() error = %v", err)
	}

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		insertContentRowForTrim(t, now.Add(-time.Duration(3-i)*time.Hour), "sk-failed", 1, compressed)
	}

	deleted, err := cleanupOversizedLogContent(getDB(), maxBytes)
	if err != nil {
		t.Fatalf("cleanupOversizedLogContent() error = %v", err)
	}
	if deleted == 0 {
		t.Fatal("cap must still be enforced when every row is a failure")
	}

	totalBytes, err := queryStoredContentBytes(getDB())
	if err != nil {
		t.Fatalf("queryStoredContentBytes() error = %v", err)
	}
	if totalBytes > maxBytes {
		t.Fatalf("total stored bytes = %d, want <= %d", totalBytes, maxBytes)
	}
}

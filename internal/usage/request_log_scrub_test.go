package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Made-up credentials in the shapes the scrubber recognises.
const (
	scrubTestProjectKey = "sk-proj-Zq4XyL8mN2vB6cR1tK9wP3sD7fG5hJ0aE4uI8oY2"
	scrubTestPassword   = "Pr0d-Hunter2!"
)

// scrubTestEntry is a request log whose prompt, response and diagnostics all
// carry the credentials above, the way an agent's do after it reads a .env.
func scrubTestEntry(key string) RequestLogEntry {
	entry := resilienceTestEntry(key, "model-scrub", 10, time.Now())
	prompt := "deploy with\nOPENAI_API_KEY=" + scrubTestProjectKey + "\nDB_PASSWORD=" + scrubTestPassword + "\n"
	input, _ := json.Marshal(map[string]any{"model": "model-scrub", "messages": []map[string]string{{"role": "user", "content": prompt}}})
	upstream := "POST https://api.openai.com/v1/responses\nAuthorization: Bearer " + scrubTestProjectKey + "\n\n" + string(input)
	detail, _ := json.Marshal(map[string]any{"upstream": map[string]string{"request_log": upstream}})
	entry.InputContent = string(input)
	entry.OutputContent = `data: {"type":"response.output_text.delta","delta":"Using ` + scrubTestProjectKey + ` now"}` + "\n\n"
	entry.DetailContent = string(detail)
	return entry
}

func assertScrubbedContent(t *testing.T, where, content string, wantJSON bool) {
	t.Helper()
	for _, secret := range []string{scrubTestProjectKey, scrubTestPassword} {
		if strings.Contains(content, secret) {
			t.Errorf("%s still holds %q:\n%s", where, secret, content)
		}
	}
	if !strings.Contains(content, "[REDACTED:") {
		t.Errorf("%s has no replacement:\n%s", where, content)
	}
	if wantJSON && !json.Valid([]byte(content)) {
		t.Errorf("%s is no longer valid JSON:\n%s", where, content)
	}
}

func onlyRequestLogID(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := getDB().QueryRow(`SELECT id FROM request_logs`).Scan(&id); err != nil {
		t.Fatalf("read request_logs id: %v", err)
	}
	return id
}

func assertStoredContentScrubbed(t *testing.T) {
	t.Helper()
	id := onlyRequestLogID(t)
	for _, part := range []string{"input", "output", "details"} {
		got, err := QueryLogContentPart(id, part)
		if err != nil {
			t.Fatalf("QueryLogContentPart(%s) error = %v", part, err)
		}
		assertScrubbedContent(t, "stored "+part, got.Content, part != "output")
	}
}

func TestInsertRequestLogStoresScrubbedContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	resetUsageWriteStateForTest(t)

	InsertRequestLog(scrubTestEntry("key-scrub"))
	assertStoredContentScrubbed(t)
}

// spooledEntries decodes every record the spool holds on disk.
func spooledEntries(t *testing.T, spool *usageSpool) []RequestLogEntry {
	t.Helper()
	spool.flush()
	paths, err := filepath.Glob(filepath.Join(spool.dir, "*"+usageSpoolSegmentSuffix))
	if err != nil {
		t.Fatalf("list spool segments: %v", err)
	}
	var entries []RequestLogEntry
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			t.Fatalf("open spool segment: %v", err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			entry, _, err := decodeUsageSpoolRecord(scanner.Bytes())
			if err != nil {
				t.Fatalf("decode spool record: %v", err)
			}
			entries = append(entries, entry)
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			t.Fatalf("read spool segment: %v", err)
		}
	}
	return entries
}

// While the database is down, records go to the spool on disk; what they
// hold there is already scrubbed.
func TestSpooledRequestLogIsScrubbed(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	usageDBHealth.markDown(errors.New("database unreachable"))

	InsertRequestLog(scrubTestEntry("key-spooled"))
	entries := spooledEntries(t, spool)
	if len(entries) != 1 {
		t.Fatalf("spooled %d records, want 1", len(entries))
	}
	assertScrubbedContent(t, "spooled input", entries[0].InputContent, true)
	assertScrubbedContent(t, "spooled output", entries[0].OutputContent, false)
	assertScrubbedContent(t, "spooled details", entries[0].DetailContent, true)
}

// A record spooled by a build that did not scrub yet is scrubbed when it is
// replayed into the database.
func TestSpoolReplayScrubsRecordsFromOlderBuilds(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	if !spoolLiveRequestLog(spool, scrubTestEntry("key-old-build"), usageSpoolReasonWriteFailed, time.Time{}, nil) {
		t.Fatal("spoolLiveRequestLog() = false")
	}
	if entries := spooledEntries(t, spool); len(entries) != 1 || !strings.Contains(entries[0].InputContent, scrubTestProjectKey) {
		t.Fatal("the record was not spooled as an older build would have")
	}

	if err := newUsageSpoolReplayer(spool).drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	assertStoredContentScrubbed(t)
}

// Content moved out of the legacy request_logs columns is scrubbed like newly
// written content.
func TestMigrateLegacyContentBatchScrubsContent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true, ContentRetentionDays: 30, CleanupIntervalMinutes: 1440})
	entry := scrubTestEntry("key-legacy")
	db := getDB()
	result, err := db.Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, model, source, channel_name, auth_index,
			 failed, latency_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
			 cost, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		"sk-legacy", "legacy-model", "legacy-source", "legacy-channel", "auth-legacy",
		0, 10, 1, 2, 0, 0, 3, 0, entry.InputContent, entry.OutputContent,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	logID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId() error = %v", err)
	}
	if migrated, err := migrateLegacyContentBatch(db, 100); err != nil || migrated != 1 {
		t.Fatalf("migrateLegacyContentBatch() = %d, %v; want 1, nil", migrated, err)
	}
	content, err := QueryLogContent(logID)
	if err != nil {
		t.Fatalf("QueryLogContent() error = %v", err)
	}
	assertScrubbedContent(t, "migrated input", content.InputContent, true)
	assertScrubbedContent(t, "migrated output", content.OutputContent, false)
}

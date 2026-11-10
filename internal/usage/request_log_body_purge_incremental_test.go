package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// The body purge used to re-read all of request_log_content on every
// maintenance pass, 4 to 22 minutes on production on 2026-11-10 with every row
// already stripped. These tests pin what replaced it: rows carry
// bodies_stripped, and the purge reads only the rows that may still hold
// bodies.

const bodyPurgeTestDetail = `{"client":{"ip":"203.0.113.8"},"upstream":{"request_log":"=== API REQUEST 1 ===\nHeaders:\nX-Test: yes\nBody:\nsecret-request"},"response":{"upstream_log":"=== API RESPONSE 1 ===\nStatus: 200\nBody:\nsecret-response"}}`

var bodyPurgeTestStorage = config.RequestLogStorageConfig{
	StoreContent:           true,
	ContentRetentionDays:   30,
	CleanupIntervalMinutes: 1440,
}

// keepBodyStorageToggle restores the process-wide toggle the test flips; tests
// that never call InitDB read it too.
func keepBodyStorageToggle(t *testing.T) {
	t.Helper()
	previous := RequestLogBodyStorageEnabled()
	t.Cleanup(func() { SetRequestLogBodyStorageEnabled(previous) })
}

func insertBodyPurgeTestLog(t *testing.T, tenantID string, failed bool, detail string) {
	t.Helper()
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: tenantID,
		APIKey:          "sk-purge", APIKeyName: "Purge", Model: "gpt-purge",
		Source: "codex", ChannelName: "Codex", AuthIndex: "auth-1",
		Failed: failed, Timestamp: time.Now().UTC(), LatencyMs: 100,
		Tokens:       TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		InputContent: `{"secret":"request"}`, OutputContent: `{"secret":"response"}`, DetailContent: detail,
	})
}

// storedContentRow is one request_log_content row as the assertions read it.
type storedContentRow struct {
	LogID          int64
	BodiesStripped int
	InputLen       int64
	OutputLen      int64
	Detail         string
}

func storedContentRows(t *testing.T, db *sql.DB) []storedContentRow {
	t.Helper()
	rows, err := db.Query(`SELECT log_id, bodies_stripped, length(input_content), length(output_content), compression, detail_content
		FROM request_log_content ORDER BY log_id`)
	if err != nil {
		t.Fatalf("query request_log_content: %v", err)
	}
	defer rows.Close()
	var out []storedContentRow
	for rows.Next() {
		var row storedContentRow
		var compression string
		var detail []byte
		if err := rows.Scan(&row.LogID, &row.BodiesStripped, &row.InputLen, &row.OutputLen, &compression, &detail); err != nil {
			t.Fatalf("scan request_log_content: %v", err)
		}
		if row.Detail, err = decompressLogContent(compression, detail); err != nil {
			t.Fatalf("decompress detail of log %d: %v", row.LogID, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate request_log_content: %v", err)
	}
	return out
}

func strippedFlags(rows []storedContentRow) []int {
	flags := make([]int, len(rows))
	for i, row := range rows {
		flags[i] = row.BodiesStripped
	}
	return flags
}

func purgeBodiesForTest(t *testing.T) RequestLogBodyPurgeResult {
	t.Helper()
	result, err := PurgeStoredRequestBodies()
	if err != nil {
		t.Fatalf("PurgeStoredRequestBodies() error = %v", err)
	}
	return result
}

func assertBodiesGone(t *testing.T, row storedContentRow) {
	t.Helper()
	if row.InputLen != 0 || row.OutputLen != 0 {
		t.Fatalf("log %d keeps bodies: input=%d output=%d bytes", row.LogID, row.InputLen, row.OutputLen)
	}
	if strings.Contains(row.Detail, "secret-request") || strings.Contains(row.Detail, "secret-response") {
		t.Fatalf("log %d detail keeps bodies: %q", row.LogID, row.Detail)
	}
	if row.Detail != "" && !strings.Contains(row.Detail, "X-Test: yes") {
		t.Fatalf("log %d detail lost its metadata: %q", row.LogID, row.Detail)
	}
}

func TestBodyPurgeReadsOnlyRowsWrittenWithStorageOn(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	db := getDB()

	// Written with storage off: stripped on the way in, so nothing to read.
	SetRequestLogBodyStorageEnabled(false)
	for i := 0; i < 3; i++ {
		insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	}
	if got := strippedFlags(storedContentRows(t, db)); !reflect.DeepEqual(got, []int{1, 1, 1}) {
		t.Fatalf("flags written with storage off = %v, want all 1", got)
	}
	if result := purgeBodiesForTest(t); result.CheckedRows != 0 || result.PhysicalReclaimDeferred {
		t.Fatalf("purge with nothing written while storage was on = %+v, want no rows read", result)
	}

	// Written while storage is on: bodies kept until it is turned off.
	SetRequestLogBodyStorageEnabled(true)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	stored := storedContentRows(t, db)
	if got := strippedFlags(stored); !reflect.DeepEqual(got, []int{1, 1, 1, 0, 0}) {
		t.Fatalf("flags = %v, want the two rows written with storage on at 0", got)
	}
	if stored[3].InputLen == 0 || !strings.Contains(stored[3].Detail, "secret-request") {
		t.Fatalf("row written with storage on lost its bodies before the purge: %+v", stored[3])
	}

	SetRequestLogBodyStorageEnabled(false)
	result := purgeBodiesForTest(t)
	if result.CheckedRows != 2 || result.ClearedBodyRows != 2 || result.SanitizedDetailRows != 2 || !result.PhysicalReclaimDeferred {
		t.Fatalf("purge after storage was on = %+v, want the 2 rows read and stripped", result)
	}
	stored = storedContentRows(t, db)
	if got := strippedFlags(stored); !reflect.DeepEqual(got, []int{1, 1, 1, 1, 1}) {
		t.Fatalf("flags after the purge = %v, want all 1", got)
	}
	for _, row := range stored {
		assertBodiesGone(t, row)
	}

	if result := purgeBodiesForTest(t); result.CheckedRows != 0 {
		t.Fatalf("second purge = %+v, want no rows read", result)
	}
}

// The toggle is one setting for the whole deployment. The old purge cleared
// input/output through clearRequestLogs, which defaults to the system tenant,
// so another tenant's bodies stayed until content retention removed them.
// Production had no rows in the system tenant at all.
func TestBodyPurgeCoversEveryTenant(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)

	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, "11f9ab9c-9fa6-4875-a76a-c39f113c57eb", false, bodyPurgeTestDetail)
	SetRequestLogBodyStorageEnabled(false)

	if result := purgeBodiesForTest(t); result.CheckedRows != 2 || result.ClearedBodyRows != 2 {
		t.Fatalf("purge = %+v, want both tenants' rows stripped", result)
	}
	for _, row := range storedContentRows(t, getDB()) {
		assertBodiesGone(t, row)
	}
}

func TestBodyPurgeKeepsFailedErrorAndDropsEmptiedRows(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	db := getDB()

	insertBodyPurgeTestLog(t, "", true, bodyPurgeTestDetail)
	// Bodies and nothing else: stripped, the row has nothing left to show.
	insertBodyPurgeTestLog(t, "", false, "")
	before := storedContentRows(t, db)
	if len(before) != 2 {
		t.Fatalf("rows before the purge = %d, want 2", len(before))
	}
	SetRequestLogBodyStorageEnabled(false)

	result := purgeBodiesForTest(t)
	if result.CheckedRows != 2 || result.ClearedBodyRows != 2 || result.DeletedContents != 1 {
		t.Fatalf("purge = %+v, want 2 rows stripped and the emptied one deleted", result)
	}
	after := storedContentRows(t, db)
	if len(after) != 1 || after[0].LogID != before[0].LogID {
		t.Fatalf("rows after the purge = %+v, want only the failed request's", after)
	}
	failed := after[0]
	if failed.InputLen != 0 || failed.OutputLen != before[0].OutputLen || failed.BodiesStripped != 1 {
		t.Fatalf("failed request row = %+v, want the request body gone and the upstream error kept", failed)
	}
	if strings.Contains(failed.Detail, "secret-request") || !strings.Contains(failed.Detail, "X-Test: yes") {
		t.Fatalf("failed request detail = %q", failed.Detail)
	}
}

// The flags are the persisted progress: a restart neither loses rows still to
// strip nor reads the stripped ones again. Each process here starts with
// storage on so its first maintenance pass leaves the purge to the test.
func TestBodyPurgeProgressSurvivesRestart(t *testing.T) {
	keepBodyStorageToggle(t)
	CloseDB()
	t.Cleanup(CloseDB)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	open := func() {
		t.Helper()
		if err := InitDB(dbPath, bodyPurgeTestStorage, time.UTC); err != nil {
			t.Fatalf("InitDB() error = %v", err)
		}
		stopRequestLogMaintenance()
	}

	open()
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	CloseDB()

	open()
	SetRequestLogBodyStorageEnabled(false)
	if result := purgeBodiesForTest(t); result.CheckedRows != 2 || result.ClearedBodyRows != 2 {
		t.Fatalf("purge in the second process = %+v, want the 2 rows from the first", result)
	}
	CloseDB()

	open()
	SetRequestLogBodyStorageEnabled(false)
	if result := purgeBodiesForTest(t); result.CheckedRows != 0 {
		t.Fatalf("purge in the third process = %+v, want no rows read", result)
	}
	for _, row := range storedContentRows(t, getDB()) {
		assertBodiesGone(t, row)
	}
}

// Rows older than bodies_stripped read 0 whatever storage was when they were
// written, so the first purge after the upgrade checks each once.
func TestBodyPurgeChecksRowsOlderThanTheFlag(t *testing.T) {
	keepBodyStorageToggle(t)
	CloseDB()
	t.Cleanup(CloseDB)
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, bodyPurgeTestStorage, time.UTC); err != nil {
		t.Fatalf("InitDB() error = %v", err)
	}
	stopRequestLogMaintenance()
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	SetRequestLogBodyStorageEnabled(false)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	CloseDB()

	// Take the database back to before the column.
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	for _, stmt := range []string{
		`DROP INDEX idx_log_content_bodies_unstripped`,
		`ALTER TABLE request_log_content DROP COLUMN bodies_stripped`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if err := InitDB(dbPath, bodyPurgeTestStorage, time.UTC); err != nil {
		t.Fatalf("InitDB() on the old schema error = %v", err)
	}
	stopRequestLogMaintenance()
	if got := strippedFlags(storedContentRows(t, getDB())); !reflect.DeepEqual(got, []int{0, 0}) {
		t.Fatalf("flags after the upgrade = %v, want 0 for both old rows", got)
	}
	SetRequestLogBodyStorageEnabled(false)
	if result := purgeBodiesForTest(t); result.CheckedRows != 2 || result.ClearedBodyRows != 1 {
		t.Fatalf("first purge after the upgrade = %+v, want both rows read, one with bodies", result)
	}
	if result := purgeBodiesForTest(t); result.CheckedRows != 0 {
		t.Fatalf("second purge after the upgrade = %+v, want no rows read", result)
	}
}

// A row rewritten in place takes the flag of its new content.
func TestBodyPurgeFlagFollowsRewrittenContent(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	db := getDB()
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	logID := storedContentRows(t, db)[0].LogID

	SetRequestLogBodyStorageEnabled(false)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := insertLogContentTx(tx, logID, time.Now().UTC(), "input again", "output again", bodyPurgeTestDetail); err != nil {
		_ = tx.Rollback()
		t.Fatalf("rewrite content: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	row := storedContentRows(t, db)[0]
	if row.BodiesStripped != 1 {
		t.Fatalf("flag after a rewrite with storage off = %d, want 1", row.BodiesStripped)
	}
	assertBodiesGone(t, row)
}

// The purge reads a batch, then writes it in a transaction of its own. A row
// rewritten in between must not be flagged on what it held before: the flag
// would keep the new bodies out of every later purge.
func TestBodyPurgeLeavesRowRewrittenAfterItWasRead(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	db := getDB()
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)
	stored := storedContentRows(t, db)
	SetRequestLogBodyStorageEnabled(false)
	ctx := context.Background()
	batch, err := loadUnstrippedContentRows(ctx, db, []int64{stored[0].LogID, stored[1].LogID})
	if err != nil || len(batch) != 2 {
		t.Fatalf("loadUnstrippedContentRows() = %d rows, error %v", len(batch), err)
	}

	// Between the read and the write, the second row gets new bodies.
	SetRequestLogBodyStorageEnabled(true)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := insertLogContentTx(tx, stored[1].LogID, time.Now().UTC(),
		"a newer and much longer request body, written after the purge read the row", "a newer response body", bodyPurgeTestDetail); err != nil {
		_ = tx.Rollback()
		t.Fatalf("rewrite content: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	SetRequestLogBodyStorageEnabled(false)

	var result RequestLogBodyPurgeResult
	if err := applyContentStrip(ctx, db, batch, &result); err != nil {
		t.Fatalf("applyContentStrip() error = %v", err)
	}
	if result.CheckedRows != 2 || result.ClearedBodyRows != 1 {
		t.Fatalf("result = %+v, want only the unchanged row stripped", result)
	}
	after := storedContentRows(t, db)
	if after[0].BodiesStripped != 1 || after[1].BodiesStripped != 0 {
		t.Fatalf("flags = %v, want the rewritten row left at 0", strippedFlags(after))
	}
	if after[1].InputLen == 0 {
		t.Fatalf("the rewritten row lost its new content to a write planned on the old one: %+v", after[1])
	}
	if result := purgeBodiesForTest(t); result.CheckedRows != 1 || result.ClearedBodyRows != 1 {
		t.Fatalf("next purge = %+v, want the rewritten row stripped", result)
	}
	for _, row := range storedContentRows(t, db) {
		assertBodiesGone(t, row)
	}
}

// Storage turned back on after the purge was asked for: what it keeps from then
// on is meant to stay, so the purge leaves every row as it is.
func TestBodyPurgeLeavesRowsWhileStorageIsOn(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	insertBodyPurgeTestLog(t, "", false, bodyPurgeTestDetail)

	result, err := purgeStoredRequestBodies(context.Background(), getDB(), "sqlite")
	if err != nil {
		t.Fatalf("purgeStoredRequestBodies() error = %v", err)
	}
	row := storedContentRows(t, getDB())[0]
	if result.CheckedRows != 0 || row.BodiesStripped != 0 || row.InputLen == 0 {
		t.Fatalf("purge with storage on = %+v, row = %+v; want the row untouched", result, row)
	}
}

func TestBodyPurgeClearsLegacyColumnsOnce(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	keepBodyStorageToggle(t)
	db := getDB()
	plantLegacyBodies := func(model string, failed int, input, output string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO request_logs (timestamp, model, failed, input_content, output_content) VALUES (?, ?, ?, ?, ?)`,
			time.Now().UTC().Format(time.RFC3339Nano), model, failed, input, output); err != nil {
			t.Fatalf("insert legacy request log: %v", err)
		}
	}
	legacyColumns := func(model string) (string, string) {
		t.Helper()
		var input, output string
		if err := db.QueryRow(`SELECT input_content, output_content FROM request_logs WHERE model = ?`, model).Scan(&input, &output); err != nil {
			t.Fatalf("read legacy columns of %s: %v", model, err)
		}
		return input, output
	}

	// What an early build stored on request_logs itself.
	plantLegacyBodies("legacy-ok", 0, "legacy input", "legacy output")
	plantLegacyBodies("legacy-failed", 1, "legacy input", "legacy error")
	SetRequestLogBodyStorageEnabled(false)

	if result := purgeBodiesForTest(t); result.ClearedLegacyRows != 2 {
		t.Fatalf("first purge = %+v, want both legacy rows cleared", result)
	}
	if input, output := legacyColumns("legacy-ok"); input != "" || output != "" {
		t.Fatalf("legacy columns = %q, %q; want both empty", input, output)
	}
	if input, output := legacyColumns("legacy-failed"); input != "" || output != "legacy error" {
		t.Fatalf("failed request legacy columns = %q, %q; want the error kept", input, output)
	}
	if got := projectionMarkerValue(db, requestLogLegacyBodiesPurgedMarker); got != requestLogLegacyBodiesPurgedDone {
		t.Fatalf("legacy purge marker = %q, want %q", got, requestLogLegacyBodiesPurgedDone)
	}

	// Nothing writes those columns any more. This row is planted only to show
	// that later purges no longer scan request_logs for them.
	plantLegacyBodies("legacy-later", 0, "planted", "planted")
	if result := purgeBodiesForTest(t); result.ClearedLegacyRows != 0 {
		t.Fatalf("second purge = %+v, want request_logs left alone", result)
	}
	if input, _ := legacyColumns("legacy-later"); input != "planted" {
		t.Fatalf("second purge scanned request_logs: input = %q", input)
	}
}

// With every row stripped the purge must not read the table to find that
// out. SQLite's planner takes the partial index only when the query needs no
// column outside it.
func TestBodyPurgeFindsRowsThroughThePartialIndex(t *testing.T) {
	initTestUsageDB(t, bodyPurgeTestStorage)
	rows, err := getDB().Query("EXPLAIN QUERY PLAN "+unstrippedRequestLogIDsSQL, 0)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(plan.String(), "COVERING INDEX idx_log_content_bodies_unstripped") {
		t.Fatalf("query plan does not read the partial index alone:\n%s", plan.String())
	}
}

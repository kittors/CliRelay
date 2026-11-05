package contentmoderation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

var recordModerators = map[string]*RuntimeModerator{}

// maskedRecord sends a request through the guard of a tenant that masks
// secrets and personal data, emails sent as written, and returns its record.
func maskedRecord(t *testing.T, tenantID, endUser, text string) *MaskRecord {
	t.Helper()
	moderator := recordModerators[tenantID]
	if moderator == nil {
		// one per tenant, as in a server: the tenant's key, and so a value's id,
		// stays the same across its requests
		moderator = maskingModerator(t)
		p := normalized(t, MaskPolicy{Secrets: true, Personal: true, Actions: map[string]string{"EMAIL": "warn"}})
		if _, err := moderator.secretMasking.SetPolicy(context.Background(), tenantID, p); err != nil {
			t.Fatal(err)
		}
		recordModerators[tenantID] = moderator
		t.Cleanup(func() { delete(recordModerators, tenantID) })
	}
	ctx, ginCtx := snapshotContext(t)
	ginCtx.Set("accessMetadata", map[string]string{"end-user-id": endUser, "api-key-id": "key-" + endUser})
	req, opts := guardOptions(tenantID, chatRequest(text))
	if result := moderator.GuardRequest(ctx, req, opts); result.Blocked {
		t.Fatalf("blocked: %+v", result)
	}
	snapshot, ok := SecretMaskingSnapshotFromGin(ginCtx)
	if !ok {
		t.Fatal("no snapshot")
	}
	NoteSecretMaskingRoute(ctx, "codex-pool", "codex")
	record := snapshot.Record("codex")
	if record == nil {
		t.Fatal("no record")
	}
	return record
}

// A record describes the values and shows the text around them as the
// upstream got it; it never holds a value, one sent as written included.
func TestMaskRecordNeverHoldsAValue(t *testing.T) {
	const email = "ada.lovelace@gmail.com"
	record := maskedRecord(t, "tenant-r", "u-1", "deploy with "+testOpenAIKey+" and tell "+email+" when done")
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(testOpenAIKey)) || bytes.Contains(raw, []byte(email)) {
		t.Fatalf("the record holds a value: %s", raw)
	}
	if record.Channel != "codex-pool" || record.Provider != "codex" || record.Places != 1 || record.Distinct != 2 || record.Warned != 1 {
		t.Fatalf("record = %+v", record)
	}
	if len(record.Snippets) != 1 || !strings.Contains(record.Snippets[0].Text, "{{API_KEY_") || !strings.Contains(record.Snippets[0].Text, "‹EMAIL›") ||
		record.Snippets[0].Role != "user" || !strings.HasPrefix(record.Snippets[0].Field, "messages") {
		t.Fatalf("snippets = %+v", record.Snippets)
	}
	for _, v := range record.Values {
		if v.Field == "" || v.ID == "" || v.Kind == "API_KEY" && v.Placeholder == "" || v.Kind == "EMAIL" && v.Placeholder != "" {
			t.Fatalf("value = %+v", v)
		}
	}
}

func testRecordWriter(store *Store) *MaskRecordWriter {
	w := &MaskRecordWriter{queue: make(chan *MaskRecord, 64)}
	w.store.Store(store)
	return w
}

func TestMaskRecordsAreCountedListedAndScoped(t *testing.T) {
	checkMaskRecordsAreCountedListedAndScoped(t, newTestStore(t))
}

// The same on PostgreSQL, in a schema of its own: the upserts, the LIKE with
// its ESCAPE and the paging are written for both.
func TestMaskRecordsAreCountedListedAndScopedPostgres(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	admin, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("pm_records_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	scoped, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	db, err := sql.Open(compatdriver.DriverName, scoped.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := InitTables(db); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db)
	checkMaskRecordsAreCountedListedAndScoped(t, store)
	// the table portal users kept their own settings in is dropped
	var users sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('privacy_mask_users')::text`).Scan(&users); err != nil || users.Valid {
		t.Fatalf("privacy_mask_users = %+v, %v", users, err)
	}
	if _, err := CleanupMaskRecords(context.Background(), db, 3, true, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func checkMaskRecordsAreCountedListedAndScoped(t *testing.T, store *Store) {
	w := testRecordWriter(store)
	first := maskedRecord(t, "tenant-s", "u-1", "key "+testOpenAIKey)
	second := maskedRecord(t, "tenant-s", "u-1", "again "+testOpenAIKey+" for ada.lovelace@gmail.com")
	other := maskedRecord(t, "tenant-s", "u-2", "mine is sk-proj-"+strings.Repeat("Z9", 20))
	foreign := maskedRecord(t, "tenant-t", "u-1", "key "+testOpenAIKey)
	second.At = first.At.Add(time.Second)
	second.Key = newMaskKey(second.At)
	for _, r := range []*MaskRecord{first, second, other, foreign} {
		w.Add(r)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	tenant := MaskScope{TenantID: "tenant-s"}

	summary, err := store.MaskSummary(ctx, tenant, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	// the same key in two requests is one value; the other user's is another
	if summary.Requests != 3 || summary.Places != 3 || summary.Distinct != 3 || summary.Warned != 1 || len(summary.Days) != 7 ||
		summary.Days[6].Requests != 3 || len(summary.Channels) != 1 || summary.Channels[0].Vendor != "codex" {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Values[0].Requests != 2 || summary.Values[0].Kind != "API_KEY" {
		t.Fatalf("top value = %+v", summary.Values[0])
	}

	page, err := store.MaskRecords(ctx, tenant, MaskRecordQuery{Days: 7, Limit: 2}, now)
	if err != nil || len(page.Items) != 2 || page.Next == "" || page.Items[0].Key < page.Items[1].Key {
		t.Fatalf("page = %+v, %v", page, err)
	}
	rest, err := store.MaskRecords(ctx, tenant, MaskRecordQuery{Days: 7, Limit: 2, Before: page.Next}, now)
	if err != nil || len(rest.Items) != 1 || rest.Next != "" {
		t.Fatalf("rest = %+v, %v", rest, err)
	}
	for _, q := range []MaskRecordQuery{{Days: 7, Facet: "warn"}, {Days: 7, Kind: "EMAIL"}, {Days: 7, Facet: "personal"}} {
		got, err := store.MaskRecords(ctx, tenant, q, now)
		if err != nil || len(got.Items) != 1 || got.Items[0].Key != second.Key {
			t.Fatalf("%+v: %+v, %v", q, got, err)
		}
	}
	if got, _ := store.MaskRecords(ctx, tenant, MaskRecordQuery{Days: 7, Kind: "API%"}, now); len(got.Items) != 0 {
		t.Fatalf("a kind matched as a pattern: %+v", got)
	}
	if page.Items[0].Values[0].Placeholder != "" {
		t.Fatal("the list shows placeholders")
	}

	// a portal user sees their own records only
	mine := MaskScope{TenantID: "tenant-s", EndUserID: "u-1"}
	if got, _ := store.MaskRecords(ctx, mine, MaskRecordQuery{Days: 7}, now); len(got.Items) != 2 {
		t.Fatalf("u-1 sees %d records", len(got.Items))
	}
	if _, err := store.MaskRecord(ctx, mine, other.Key); !errors.Is(err, ErrMaskRecordNotFound) {
		t.Fatalf("u-1 read u-2's record: %v", err)
	}
	if _, err := store.MaskRecord(ctx, tenant, foreign.Key); !errors.Is(err, ErrMaskRecordNotFound) {
		t.Fatalf("a tenant read another tenant's record: %v", err)
	}
	detail, err := store.MaskRecord(ctx, mine, second.Key)
	if err != nil || len(detail.Snippets) != 1 || detail.Values[0].Placeholder == "" || detail.Values[0].ID != "" {
		t.Fatalf("detail = %+v, %v", detail, err)
	}

	deleted, err := store.DeleteMaskRecords(ctx, mine)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted %d, %v", deleted, err)
	}
	if summary, _ := store.MaskSummary(ctx, tenant, 7, now); summary.Requests != 1 {
		t.Fatalf("after deleting u-1's: %+v", summary)
	}

	// deleting a portal user takes their records and day counts with them;
	// another tenant's user of the same id keeps theirs
	if err := store.DeleteMaskUserRecords(ctx, "tenant-s", "u-2"); err != nil {
		t.Fatal(err)
	}
	if summary, _ := store.MaskSummary(ctx, tenant, 7, now); summary.Requests != 0 || summary.Places != 0 {
		t.Fatalf("after deleting u-2: %+v", summary)
	}
	if summary, _ := store.MaskSummary(ctx, MaskScope{TenantID: "tenant-t"}, 7, now); summary.Requests != 1 {
		t.Fatalf("another tenant's records went too: %+v", summary)
	}
}

func TestCleanupMaskRecordsKeepsThirtyDaysAndClearsSnippets(t *testing.T) {
	store := newTestStore(t)
	w := testRecordWriter(store)
	now := time.Now()
	old := maskedRecord(t, "tenant-c", "", "key "+testOpenAIKey)
	old.At = now.Add(-MaskRecordRetention - time.Hour)
	old.Key = newMaskKey(old.At)
	week := maskedRecord(t, "tenant-c", "", "key "+testOpenAIKey)
	week.At = now.Add(-7 * 24 * time.Hour)
	week.Key = newMaskKey(week.At)
	fresh := maskedRecord(t, "tenant-c", "", "key "+testOpenAIKey)
	for _, r := range []*MaskRecord{old, week, fresh} {
		w.Add(r)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	ctx, scope := context.Background(), MaskScope{TenantID: "tenant-c"}
	deleted, err := CleanupMaskRecords(ctx, store.db, 3, true, now)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted %d, %v", deleted, err)
	}
	if _, err := store.MaskRecord(ctx, scope, old.Key); !errors.Is(err, ErrMaskRecordNotFound) {
		t.Fatal("a record past the retention is still there")
	}
	if got, _ := store.MaskRecord(ctx, scope, week.Key); len(got.Snippets) != 0 {
		t.Fatal("a snippet past the content retention is still there")
	}
	if got, _ := store.MaskRecord(ctx, scope, fresh.Key); len(got.Snippets) != 1 {
		t.Fatal("a fresh record lost its snippet")
	}
	if _, err := CleanupMaskRecords(ctx, store.db, 3, false, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.MaskRecord(ctx, scope, fresh.Key); len(got.Snippets) != 0 {
		t.Fatal("snippets stayed while bodies are not stored")
	}
}

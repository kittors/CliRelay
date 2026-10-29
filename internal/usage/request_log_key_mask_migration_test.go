package usage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// fastRequestLogKeyMasking removes every wait from the masking pass and shrinks
// its batches to two rows, so a handful of rows exercises batching. The
// production values are restored when the test ends.
func fastRequestLogKeyMasking(t *testing.T) {
	t.Helper()
	size, batchPause, retryPause, settlePause := requestLogKeyMaskBatchSize, requestLogKeyMaskBatchPause, requestLogKeyMaskRetryPause, requestLogKeyMaskSettlePause
	sleep := requestLogKeyMaskSleep
	requestLogKeyMaskBatchSize = 2
	requestLogKeyMaskBatchPause = 0
	requestLogKeyMaskRetryPause = 0
	requestLogKeyMaskSettlePause = 0
	t.Cleanup(func() {
		requestLogKeyMaskBatchSize, requestLogKeyMaskBatchPause = size, batchPause
		requestLogKeyMaskRetryPause, requestLogKeyMaskSettlePause = retryPause, settlePause
		requestLogKeyMaskSleep = sleep
	})
}

// waitForRequestLogKeyMaskingToStop waits for the scheduler goroutine to return.
func waitForRequestLogKeyMaskingToStop(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for requestLogKeyMaskScheduled.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the masking scheduler did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countRequestLogsWhere(t *testing.T, where string, args ...any) int {
	t.Helper()
	n, err := queryRequestLogsWhereCount(where, args...)
	if err != nil {
		t.Fatalf("count request_logs where %s: %v", where, err)
	}
	return n
}

// queryRequestLogsWhereCount is the variant for goroutines other than the test's,
// which must not call t.Fatalf.
func queryRequestLogsWhereCount(where string, args ...any) (int, error) {
	var n int
	err := getDB().QueryRow("SELECT COUNT(*) FROM request_logs WHERE "+where, args...).Scan(&n)
	return n, err
}

// legacyKeyMaskFixture writes the rows an older build left behind and returns
// the secrets involved.
type legacyKeyMaskFixture struct {
	secretA, secretB, deleted string
}

func seedLegacyKeyMaskRows(t *testing.T) legacyKeyMaskFixture {
	t.Helper()
	f := legacyKeyMaskFixture{secretA: "sk-mig-secret-a-0001", secretB: "sk-mig-secret-b-0002", deleted: "sk-mig-deleted-0003"}
	registerTestAPIKey(t, "mig-key-a", f.secretA, "A")
	registerTestAPIKey(t, "mig-key-b", f.secretB, "B")

	at := time.Now().UTC().Add(-time.Hour)
	n := 0
	next := func() time.Time { n++; return at.Add(time.Duration(n) * time.Second) }
	// Secret and no id: the id has to be found through api_keys.
	for i := 0; i < 5; i++ {
		insertRawRequestLog(t, f.secretA, "", "A snapshot", next())
	}
	// Secret and an id already: the id is kept.
	for i := 0; i < 2; i++ {
		insertRawRequestLog(t, f.secretA, "mig-key-a", "A snapshot", next())
	}
	// Secret whose row names another key (the secret was reassigned since): the
	// row keeps the attribution it was written with.
	insertRawRequestLog(t, f.secretA, "mig-key-old-owner", "Old owner", next())
	for i := 0; i < 3; i++ {
		insertRawRequestLog(t, f.secretB, "", "", next())
	}
	// A key that no longer exists: nothing to attribute, the name snapshot stays.
	for i := 0; i < 2; i++ {
		insertRawRequestLog(t, f.deleted, "", "Deleted user", next())
	}
	// Already masked, labels and no key at all: none of these is touched.
	insertRawRequestLog(t, MaskAPIKey(f.secretA), "mig-key-a", "A snapshot", next())
	insertRawRequestLog(t, "POST /v1/models", "", "", next())
	insertRawRequestLog(t, "POST /v1/models", "", "", next())
	insertRawRequestLog(t, "warmup", "", "", next())
	insertRawRequestLog(t, "", "", "", next())
	return f
}

// assertLegacyKeyMaskRowsMasked is the end state after the pass has run.
func assertLegacyKeyMaskRowsMasked(t *testing.T, f legacyKeyMaskFixture) {
	t.Helper()
	for _, secret := range []string{f.secretA, f.secretB, f.deleted} {
		if n := countRequestLogsWhere(t, "api_key = ?", secret); n != 0 {
			t.Fatalf("%d request_logs rows still hold the secret %q", n, secret)
		}
	}
	// Key A: 5 filled, 2 already attributed, 1 attributed elsewhere, 1 pre-masked.
	maskA := MaskAPIKey(f.secretA)
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = ?", maskA, "mig-key-a"); n != 5+2+1 {
		t.Fatalf("key A rows attributed to it = %d, want 8", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = ?", maskA, "mig-key-old-owner"); n != 1 {
		t.Fatalf("a row written for another key lost its attribution (%d rows)", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = ?", MaskAPIKey(f.secretB), "mig-key-b"); n != 3 {
		t.Fatalf("key B rows attributed to it = %d, want 3", n)
	}
	// The deleted key: masked, no id, snapshot name kept.
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = '' AND api_key_name = ?", MaskAPIKey(f.deleted), "Deleted user"); n != 2 {
		t.Fatalf("unattributable rows = %d, want 2 keeping their name snapshot", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", "POST /v1/models"); n != 2 {
		t.Fatalf("route label rows = %d, want 2 untouched", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", "warmup"); n != 1 {
		t.Fatalf("origin label rows = %d, want 1 untouched", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ''"); n != 1 {
		t.Fatalf("keyless rows = %d, want 1 untouched", n)
	}
}

// assertRequestLogKeyMaskingEndToEnd runs on SQLite and on PostgreSQL.
func assertRequestLogKeyMaskingEndToEnd(t *testing.T) {
	t.Helper()
	fastRequestLogKeyMasking(t)
	f := seedLegacyKeyMaskRows(t)
	db := getDB()

	if err := runRequestLogAPIKeyMasking(context.Background(), db); err != nil {
		t.Fatalf("runRequestLogAPIKeyMasking: %v", err)
	}
	assertLegacyKeyMaskRowsMasked(t, f)
	if got := projectionMarkerValue(db, requestLogKeyMaskMarker); got != requestLogKeyMaskDone {
		t.Fatalf("marker = %q, want %q", got, requestLogKeyMaskDone)
	}

	// Nothing is left for a second sweep, so a repeated run is a no-op.
	if n, err := maskRequestLogAPIKeysOnce(context.Background(), db); err != nil || n != 0 {
		t.Fatalf("second sweep masked %d rows (err %v), want 0", n, err)
	}

	// A finished pass never runs again, even if a plaintext row shows up later
	// (the read path masks it for display regardless).
	insertRawRequestLog(t, f.secretA, "", "", time.Now().UTC())
	if err := runRequestLogAPIKeyMasking(context.Background(), db); err != nil {
		t.Fatalf("runRequestLogAPIKeyMasking after done: %v", err)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", f.secretA); n != 1 {
		t.Fatalf("a finished pass touched rows again: %d plaintext rows, want the 1 inserted afterwards", n)
	}
	logs, err := QueryLogs(LogQueryParams{Page: 1, Size: 200, Days: 7})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	for _, item := range logs.Items {
		if item.APIKey == f.secretA || item.APIKeyMasked == f.secretA {
			t.Fatalf("a read returned the secret: %#v", item)
		}
	}
}

func TestRequestLogKeyMaskingMasksFillsIDsAndLeavesLabels(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertRequestLogKeyMaskingEndToEnd(t)
}

func TestRequestLogKeyMaskingResumesWhereItStopped(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	fastRequestLogKeyMasking(t)
	f := seedLegacyKeyMaskRows(t)
	db := getDB()

	// One batch only, as if the process stopped after it: two rows are done and
	// the rest still hold the secret. The state is in the data, not in memory.
	n, err := maskRequestLogAPIKeyBatch(context.Background(), db, f.secretA, MaskAPIKey(f.secretA), "mig-key-a")
	if err != nil || n != 2 {
		t.Fatalf("first batch masked %d rows (err %v), want exactly the batch size", n, err)
	}
	if left := countRequestLogsWhere(t, "api_key = ?", f.secretA); left != 5+2+1-2 {
		t.Fatalf("plaintext rows of key A after one batch = %d, want 6", left)
	}

	// A cancelled run changes nothing and does not claim to be done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runRequestLogAPIKeyMasking(ctx, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run returned %v, want context.Canceled", err)
	}
	if left := countRequestLogsWhere(t, "api_key = ?", f.secretA); left != 6 {
		t.Fatalf("a cancelled run masked rows: %d left, want 6", left)
	}
	if projectionMarkerValue(db, requestLogKeyMaskMarker) == requestLogKeyMaskDone {
		t.Fatal("a cancelled run wrote the done marker")
	}

	// Running again simply carries on.
	if err := runRequestLogAPIKeyMasking(context.Background(), db); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	assertLegacyKeyMaskRowsMasked(t, f)
	if projectionMarkerValue(db, requestLogKeyMaskMarker) != requestLogKeyMaskDone {
		t.Fatal("the resumed run did not write the done marker")
	}
}

func TestRequestLogKeyMaskingNeverOverwritesAnExistingKeyID(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	fastRequestLogKeyMasking(t)
	registerTestAPIKey(t, "owner-now", "sk-reassigned-0001", "Owner now")
	insertRawRequestLog(t, "sk-reassigned-0001", "owner-then", "Owner then", time.Now().UTC())

	if err := runRequestLogAPIKeyMasking(context.Background(), getDB()); err != nil {
		t.Fatalf("runRequestLogAPIKeyMasking: %v", err)
	}
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = ?", MaskAPIKey("sk-reassigned-0001"), "owner-then"); n != 1 {
		t.Fatalf("the row was re-attributed: %d rows still name their original key", n)
	}
}

func TestRequestLogKeyMaskingKeepsASecretThatReadsLikeALabelOnlyWhenNoKeyOwnsIt(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	fastRequestLogKeyMasking(t)
	// "system" is an internal origin label, but here a stored key has that exact
	// secret: the rows are masked and attributed. Without such a key it is a
	// label and stays.
	registerTestAPIKey(t, "label-like-key", "system", "Looks like a label")
	insertRawRequestLog(t, "system", "", "", time.Now().UTC())
	insertRawRequestLog(t, "codex", "", "", time.Now().UTC())

	if err := runRequestLogAPIKeyMasking(context.Background(), getDB()); err != nil {
		t.Fatalf("runRequestLogAPIKeyMasking: %v", err)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", "system"); n != 0 {
		t.Fatalf("%d rows still hold the stored key's secret", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ? AND api_key_id = ?", MaskAPIKey("system"), "label-like-key"); n != 1 {
		t.Fatalf("masked and attributed rows = %d, want 1", n)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", "codex"); n != 1 {
		t.Fatalf("label rows = %d, want the origin label untouched", n)
	}
}

func TestInitDBDoesNotMaskRequestLogKeysInline(t *testing.T) {
	// Startup and /readyz must not wait for the pass: opening the database leaves
	// the rows alone, and only the scheduled background run touches them.
	CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	stopRequestLogMaintenance()
	insertRawRequestLog(t, "sk-startup-secret-0001", "", "", time.Now().UTC())
	CloseDB()

	if err := InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("second InitDB: %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
	if n := countRequestLogsWhere(t, "api_key = ?", "sk-startup-secret-0001"); n != 1 {
		t.Fatalf("InitDB changed the legacy row (%d plaintext rows left, want 1)", n)
	}
}

func TestRequestLogKeyMaskingDefaultsAreGentle(t *testing.T) {
	// The previous build keeps serving, and writing secrets, for its drain window
	// after a blue-green cut-over (DRAIN_SECONDS 180 + CLIRELAY_SHUTDOWN_GRACE 300,
	// counted from a cut-over that itself follows a readiness wait of up to ~90s).
	// The first pass must start well after that.
	if requestLogKeyMaskStartDelay < 10*time.Minute {
		t.Errorf("start delay = %s, want at least 10 minutes", requestLogKeyMaskStartDelay)
	}
	// Small batches with a pause in between keep row locks and WAL bursts short.
	if requestLogKeyMaskBatchSize <= 0 || requestLogKeyMaskBatchSize > 1000 {
		t.Errorf("batch size = %d, want 1..1000", requestLogKeyMaskBatchSize)
	}
	if requestLogKeyMaskBatchPause < 100*time.Millisecond {
		t.Errorf("batch pause = %s, want a real pause between batches", requestLogKeyMaskBatchPause)
	}
	if requestLogKeyMaskBatchTimeout <= 0 || requestLogKeyMaskBatchTimeout > 2*time.Minute {
		t.Errorf("batch timeout = %s, want a bounded statement", requestLogKeyMaskBatchTimeout)
	}
}

func TestScheduleRequestLogAPIKeyMaskingWaitsForTheOldSlotToStop(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	startDelay := requestLogKeyMaskStartDelay
	fastRequestLogKeyMasking(t)
	f := seedLegacyKeyMaskRows(t)
	db := getDB()

	var waits []time.Duration
	untouchedAtFirstWait := false
	requestLogKeyMaskSleep = func(d time.Duration) {
		if len(waits) == 0 {
			n, err := queryRequestLogsWhereCount("api_key = ?", f.secretA)
			untouchedAtFirstWait = err == nil && n == 5+2+1
		}
		waits = append(waits, d)
	}

	ScheduleRequestLogAPIKeyMasking()
	waitForRequestLogKeyMaskingToStop(t)

	if len(waits) != 1 || waits[0] != startDelay {
		t.Fatalf("waits = %v, want exactly the start delay %s", waits, startDelay)
	}
	if !untouchedAtFirstWait {
		t.Fatal("rows were masked before the start delay elapsed")
	}
	assertLegacyKeyMaskRowsMasked(t, f)
	if got := projectionMarkerValue(db, requestLogKeyMaskMarker); got != requestLogKeyMaskDone {
		t.Fatalf("marker = %q, want %q", got, requestLogKeyMaskDone)
	}

	// Scheduled again after it finished: it waits out the delay like any start,
	// finds the marker and does nothing.
	insertRawRequestLog(t, f.secretB, "", "", time.Now().UTC())
	waits = nil
	ScheduleRequestLogAPIKeyMasking()
	waitForRequestLogKeyMaskingToStop(t)
	if len(waits) != 1 {
		t.Fatalf("second schedule waits = %v", waits)
	}
	if n := countRequestLogsWhere(t, "api_key = ?", f.secretB); n != 1 {
		t.Fatalf("a finished pass ran again (%d plaintext rows, want the 1 inserted afterwards)", n)
	}
}

func TestScheduleRequestLogAPIKeyMaskingRunsOnTheLeaderOnly(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	fastRequestLogKeyMasking(t)
	f := seedLegacyKeyMaskRows(t)

	hub := cluster.NewMemoryHub()
	leader, follower := hub.Join("node-a"), hub.Join("node-b")
	cluster.SetDefault(follower)
	t.Cleanup(func() { cluster.SetDefault(nil) })

	waits := 0
	maskedWhileFollower := false
	requestLogKeyMaskSleep = func(time.Duration) {
		waits++
		// wait 1 is the start delay, waits 2 and 3 are follower polls; the node
		// becomes the leader after the third.
		if n, err := queryRequestLogsWhereCount("api_key = ?", f.secretA); waits >= 2 && (err != nil || n != 5+2+1) {
			maskedWhileFollower = true
		}
		if waits == 3 {
			cluster.SetDefault(leader)
		}
	}

	ScheduleRequestLogAPIKeyMasking()
	waitForRequestLogKeyMaskingToStop(t)

	if maskedWhileFollower {
		t.Fatal("a follower masked rows")
	}
	if waits != 3 {
		t.Fatalf("waits = %d, want the start delay plus two follower polls", waits)
	}
	assertLegacyKeyMaskRowsMasked(t, f)
}

func TestScheduleRequestLogAPIKeyMaskingRetriesAfterAFailure(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	fastRequestLogKeyMasking(t)
	f := seedLegacyKeyMaskRows(t)
	db := getDB()

	// Make the first attempt fail inside the pass, then let it recover.
	if _, err := db.Exec(`CREATE TRIGGER fail_request_log_masking BEFORE UPDATE ON request_logs
		BEGIN SELECT RAISE(ABORT, 'simulated failure'); END`); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}
	waits := 0
	requestLogKeyMaskSleep = func(time.Duration) {
		waits++
		if waits == 2 { // the first retry pause
			if _, err := db.Exec(`DROP TRIGGER fail_request_log_masking`); err != nil {
				t.Errorf("drop failing trigger: %v", err)
			}
		}
	}

	ScheduleRequestLogAPIKeyMasking()
	waitForRequestLogKeyMaskingToStop(t)

	if waits != 2 {
		t.Fatalf("waits = %d, want the start delay and one retry pause", waits)
	}
	assertLegacyKeyMaskRowsMasked(t, f)
}

package usage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func awaitUsageLifecycle(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out: " + what)
	}
}
func lifecycleRowCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err = db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCloseDBDrainsQueuedUsageIntoOriginalStore(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	oldPath := GetDBPath()
	original := writeRequestLogAttemptFunc
	started, release := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	var first atomic.Bool
	writeRequestLogAttemptFunc = func(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return original(entry, timeout)
	}
	t.Cleanup(func() { unblock(); coreusage.WithDrainedDefault(func() { writeRequestLogAttemptFunc = original }) })
	for i := 0; i < 20; i++ {
		coreusage.PublishRecord(context.Background(), coreusage.Record{Model: "lifecycle-test", RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, InputContent: "before close", OutputContent: "persist me"})
	}
	awaitUsageLifecycle(t, started, "first write")
	closed := make(chan struct{})
	go func() { CloseDB(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("CloseDB returned while admitted write was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	awaitUsageLifecycle(t, closed, "queue drained and DB closed")
	if n := lifecycleRowCount(t, oldPath); n != 20 {
		t.Fatalf("old store rows=%d want20", n)
	}
	oldDB, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oldDB.Close() }()
	var contentRows int
	if err = oldDB.QueryRow("SELECT COUNT(*) FROM request_log_content").Scan(&contentRows); err != nil {
		t.Fatal(err)
	}
	if contentRows != 20 {
		t.Fatalf("content rows=%d want20", contentRows)
	}
	var compression string
	var input, output []byte
	if err = oldDB.QueryRow("SELECT compression, input_content, output_content FROM request_log_content LIMIT 1").Scan(&compression, &input, &output); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		body []byte
		want string
	}{{input, "before close"}, {output, "persist me"}} {
		got, err := decompressLogContent(compression, item.body)
		if err != nil || got != item.want {
			t.Fatalf("persisted body=%q error=%v want=%q", got, err, item.want)
		}
	}
	var requests, tokens int64
	if err = oldDB.QueryRow("SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(total_tokens),0) FROM usage_rollup_buckets WHERE bucket_kind = ?", rollupBucketLifetime).Scan(&requests, &tokens); err != nil {
		t.Fatal(err)
	}
	if requests != 20 || tokens != 100 {
		t.Fatalf("durable rollup requests=%d tokens=%d", requests, tokens)
	}

	newPath := filepath.Join(t.TempDir(), "new.db")
	if err := InitDB(newPath, config.RequestLogStorageConfig{}, time.FixedZone("new", 9*3600)); err != nil {
		t.Fatal(err)
	}
	if n := countRequestLogRows(t); n != 0 {
		t.Fatalf("old queued records crossed into new store: %d", n)
	}
	// The default manager is still usable after CloseDB; no Stop/reset workaround.
	coreusage.PublishRecord(context.Background(), coreusage.Record{Model: "new-store", RequestedAt: time.Now(), Detail: coreusage.Detail{TotalTokens: 1}})
	CloseDB()
	if n := lifecycleRowCount(t, newPath); n != 1 {
		t.Fatalf("new store rows=%d want1", n)
	}
}

func TestCloseDBPinsDirectRetryUntilItsOldStoreCommit(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	oldPath := GetDBPath()
	resetUsageWriteStateForTest(t)
	original := writeRequestLogAttemptFunc
	retryStarted, release := make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	writeRequestLogAttemptFunc = func(e RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		if calls.Add(1) == 1 {
			return requestLogWriteResult{}, errors.New("connection reset by peer")
		}
		close(retryStarted)
		<-release
		return original(e, timeout)
	}
	finished := make(chan struct{})
	t.Cleanup(func() { unblock(); awaitUsageLifecycle(t, finished, "retry cleanup") })
	go func() {
		_, outcome, err := writeRequestLogWithRetry(resilienceTestEntry("lifecycle-retry", "old", 5, time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)), time.Second)
		if outcome != requestLogWriteCommitted || err != nil {
			t.Errorf("retry outcome=%v err=%v", outcome, err)
		}
		close(finished)
	}()
	awaitUsageLifecycle(t, retryStarted, "retry attempt")
	closed := make(chan struct{})
	go func() { CloseDB(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("CloseDB interrupted active retry")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	awaitUsageLifecycle(t, finished, "old write")
	awaitUsageLifecycle(t, closed, "close")
	if n := lifecycleRowCount(t, oldPath); n != 1 {
		t.Fatalf("old store rows=%d", n)
	}
	if err := InitDB(filepath.Join(t.TempDir(), "other.db"), config.RequestLogStorageConfig{}, time.FixedZone("other", 8*3600)); err != nil {
		t.Fatal(err)
	}
	oldDB, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oldDB.Close() }()
	var day string
	if err = oldDB.QueryRow("SELECT bucket_start FROM usage_rollup_buckets WHERE bucket_kind = ?", rollupBucketDay).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if day != "2026-01-01" {
		t.Fatalf("old UTC store inherited new +08 zone: day=%q", day)
	}
	if n := countRequestLogRows(t); n != 0 {
		t.Fatalf("retry crossed database epoch: %d", n)
	}
}

func TestUsageMetadataIsBoundDuringBootstrapAndRebuild(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	db := getDB()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	bound := usageMetadataTx{usageWriteTx: tx, metadata: metadataForUsageDB(db)}
	// These helpers used to acquire configuration through process globals. They
	// must also work under InitDB's metadata lock without recursively locking it.
	usageDBMu.Lock()
	err = projectUsageRollupTx(bound, rollupEvent{TenantID: "00000000-0000-0000-0000-000000000001", Model: "bootstrap", At: time.Now()})
	usageDBMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	InsertRequestLog(resilienceTestEntry("backfill-entry", "backfill", 3, time.Now()))
	if err = RunUsageRollupBackfillAtInit(); err != nil {
		t.Fatal(err)
	}
	requests, tokens := lifetimeRollupTotals(t)
	if requests != 1 || tokens != 4 {
		t.Fatalf("rebuilt requests=%d tokens=%d", requests, tokens)
	}
}

func TestUsageLifecycleReopenHistoricalBackfill(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	path := GetDBPath()
	at := time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)
	InsertRequestLog(resilienceTestEntry("historical-reopen", "historical", 8, at))
	if _, err := getDB().Exec("DELETE FROM usage_rollup_buckets"); err != nil {
		t.Fatal(err)
	}
	if _, err := getDB().Exec("DELETE FROM usage_projection_markers WHERE marker_key = ?", usageRollupBackfillMarker); err != nil {
		t.Fatal(err)
	}
	CloseDB()
	// Actual reopen followed by the same historical rebuild startup uses. A new
	// project timezone must apply to this explicit rebuild, not to the old write.
	if err := InitDB(path, config.RequestLogStorageConfig{StoreContent: true}, time.FixedZone("project", 8*3600)); err != nil {
		t.Fatal(err)
	}
	stopRequestLogMaintenance()
	if err := RunUsageRollupBackfillAtInit(); err != nil {
		t.Fatal(err)
	}
	var day string
	var count, total int64
	if err := getDB().QueryRow("SELECT bucket_start, request_count, total_tokens FROM usage_rollup_buckets WHERE bucket_kind = ?", rollupBucketDay).Scan(&day, &count, &total); err != nil {
		t.Fatal(err)
	}
	if day != "2026-01-02" || count != 1 || total != 9 {
		t.Fatalf("reopen backfill day=%q count=%d total=%d", day, count, total)
	}
	if err := RunUsageRollupBackfillAtInit(); err != nil {
		t.Fatal(err)
	}
	if n, tokens := lifetimeRollupTotals(t); n != 1 || tokens != 9 {
		t.Fatalf("repeat rebuild double counted: %d/%d", n, tokens)
	}
}

// Older package tests own a plain SQL transaction. Bind its test store snapshot
// explicitly rather than retaining a production fallback to current globals.
func usageTestMetadataTx(tx usageWriteTx) usageMetadataTx {
	return usageMetadataTx{usageWriteTx: tx, metadata: metadataForUsageDB(getDB())}
}

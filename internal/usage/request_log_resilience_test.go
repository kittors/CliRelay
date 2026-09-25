package usage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// errFakeAdminShutdown is what a client sees when Patroni fences the primary.
var errFakeAdminShutdown = &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}

// resetUsageWriteStateForTest restores the write path's process-wide state
// and shortens its timings so retry budgets run out in milliseconds.
func resetUsageWriteStateForTest(t *testing.T) {
	t.Helper()
	prevBudget, prevBackoff, prevMaxBackoff := requestLogLiveRetryBudget, requestLogRetryInitialBackoff, requestLogRetryMaxBackoff
	prevSettle, prevAttempt, prevCallback := requestLogCommitSettleWindow, writeRequestLogAttemptFunc, tokenUsageCallback
	requestLogLiveRetryBudget = 300 * time.Millisecond
	requestLogRetryInitialBackoff = 10 * time.Millisecond
	requestLogRetryMaxBackoff = 50 * time.Millisecond
	requestLogCommitSettleWindow = 300 * time.Millisecond
	usageDBHealth.downSince.Store(0)
	usageWriteShuttingDown.Store(false)
	t.Cleanup(func() {
		requestLogLiveRetryBudget, requestLogRetryInitialBackoff, requestLogRetryMaxBackoff = prevBudget, prevBackoff, prevMaxBackoff
		requestLogCommitSettleWindow, writeRequestLogAttemptFunc, tokenUsageCallback = prevSettle, prevAttempt, prevCallback
		usageDBHealth.downSince.Store(0)
		usageWriteShuttingDown.Store(false)
	})
}

// useUsageSpoolForTest installs a spool without its background replayer, so a
// test decides when replay happens.
func useUsageSpoolForTest(t *testing.T, dir string, maxBytes int64) *usageSpool {
	t.Helper()
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "usage-spool")
	}
	spool, err := openUsageSpool(dir, maxBytes)
	if err != nil {
		t.Fatalf("openUsageSpool() error = %v", err)
	}
	usageSpoolCurrent.Store(&usageSpoolRuntime{spool: spool, replayer: newUsageSpoolReplayer(spool)})
	t.Cleanup(func() {
		usageSpoolCurrent.Store(nil)
		spool.close()
	})
	return spool
}

type tokenUsageRecorder struct {
	mu    sync.Mutex
	calls []string
	total int64
}

func captureTokenUsageForTest(t *testing.T) *tokenUsageRecorder {
	t.Helper()
	recorder := &tokenUsageRecorder{}
	SetTokenUsageCallback(func(apiKey, endUserID string, totalTokens int64) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.calls = append(recorder.calls, apiKey+"|"+endUserID)
		recorder.total += totalTokens
	})
	return recorder
}

func (r *tokenUsageRecorder) snapshot() (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls), r.total
}

func resilienceTestEntry(key, model string, tokens int64, at time.Time) RequestLogEntry {
	return RequestLogEntry{
		IdempotencyKey: key,
		APIKey:         "sk-resilience-XXXX",
		Model:          model,
		Source:         "test-source",
		ChannelName:    "test-channel",
		Timestamp:      at,
		LatencyMs:      25,
		Tokens:         TokenStats{InputTokens: tokens, OutputTokens: 1, TotalTokens: tokens + 1},
	}
}

func countRequestLogRows(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatalf("count request_logs: %v", err)
	}
	return n
}

func lifetimeRollupTotals(t *testing.T) (requests, tokens int64) {
	t.Helper()
	if err := getDB().QueryRow(`SELECT COALESCE(SUM(request_count), 0), COALESCE(SUM(total_tokens), 0)
		FROM usage_rollup_buckets WHERE bucket_kind = ?`, rollupBucketLifetime).Scan(&requests, &tokens); err != nil {
		t.Fatalf("read lifetime rollup: %v", err)
	}
	return requests, tokens
}

func requestLogModelsInOrder(t *testing.T) []string {
	t.Helper()
	rows, err := getDB().Query(`SELECT model FROM request_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("list request_logs: %v", err)
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			t.Fatalf("scan request_logs: %v", err)
		}
		models = append(models, model)
	}
	return models
}

func TestIsTransientDBErrorSeparatesOutagesFromRejectedRows(t *testing.T) {
	transient := []error{
		errFakeAdminShutdown,
		&pgconn.PgError{Code: "57P02"},
		&pgconn.PgError{Code: "57P03", Message: "the database system is starting up"},
		&pgconn.PgError{Code: "08006"},
		&pgconn.PgError{Code: "25006", Message: "cannot execute INSERT in a read-only transaction"},
		&pgconn.PgError{Code: "53300"},
		fmt.Errorf("insert log: %w", io.ErrUnexpectedEOF),
		io.EOF,
		driver.ErrBadConn,
		sql.ErrConnDone,
		context.DeadlineExceeded,
		&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
		fmt.Errorf("begin insert tx: %w", syscall.ECONNRESET),
		errors.New("sql: database is closed"),
		errors.New("WARNING: The transaction has already committed locally, but might not have been replicated to the standby."),
		&commitUncertainError{err: io.ErrUnexpectedEOF},
		errUsageDBUnavailable,
	}
	for _, err := range transient {
		if !isTransientDBError(err) {
			t.Errorf("isTransientDBError(%v) = false, want true", err)
		}
	}
	permanent := []error{
		&pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"},
		&pgconn.PgError{Code: "42P01", Message: "relation does not exist"},
		&pgconn.PgError{Code: "40P01", Message: "deadlock detected"},
		errors.New("insert log: value too long"),
		nil,
	}
	for _, err := range permanent {
		if isTransientDBError(err) {
			t.Errorf("isTransientDBError(%v) = true, want false", err)
		}
	}
}

func TestInsertRequestLogRetriesConnectionErrorsThenSpools(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	tpm := captureTokenUsageForTest(t)
	var attempts atomic.Int32
	writeRequestLogAttemptFunc = func(RequestLogEntry, time.Duration) (requestLogWriteResult, error) {
		attempts.Add(1)
		return requestLogWriteResult{}, fmt.Errorf("begin insert tx: %w", errFakeAdminShutdown)
	}

	started := time.Now()
	InsertRequestLog(resilienceTestEntry("key-a", "model-a", 10, time.Now()))
	if elapsed := time.Since(started); elapsed > 5*requestLogLiveRetryBudget {
		t.Fatalf("InsertRequestLog took %s, want about the %s retry budget", elapsed, requestLogLiveRetryBudget)
	}
	if got := attempts.Load(); got < 3 {
		t.Fatalf("attempts = %d, want the connection error retried", got)
	}
	if records, _, _ := spool.pending(); records != 1 {
		t.Fatalf("spooled records = %d, want 1", records)
	}
	if !usageDBHealth.down() {
		t.Fatal("outage flag not raised after the retry budget ran out")
	}
	if calls, total := tpm.snapshot(); calls != 1 || total != 11 {
		t.Fatalf("TPM calls = %d total = %d, want the spooled record counted once", calls, total)
	}

	// With the outage known, the next record skips the database entirely.
	before := attempts.Load()
	InsertRequestLog(resilienceTestEntry("key-b", "model-b", 20, time.Now()))
	if got := attempts.Load(); got != before {
		t.Fatalf("attempts grew from %d to %d during a known outage", before, got)
	}
	if records, _, _ := spool.pending(); records != 2 {
		t.Fatalf("spooled records = %d, want 2", records)
	}
	if n := countRequestLogRows(t); n != 0 {
		t.Fatalf("request_logs rows = %d, want 0 while the database is down", n)
	}
}

func TestInsertRequestLogCommitsOnceTheDatabaseReturns(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	tpm := captureTokenUsageForTest(t)
	failures := 2
	writeRequestLogAttemptFunc = func(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		if failures > 0 {
			failures--
			return requestLogWriteResult{}, fmt.Errorf("begin insert tx: %w", io.ErrUnexpectedEOF)
		}
		return writeRequestLogAttempt(entry, timeout)
	}

	InsertRequestLog(resilienceTestEntry("key-a", "model-a", 10, time.Now()))
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want 1", n)
	}
	if records, _, _ := spool.pending(); records != 0 {
		t.Fatalf("spooled records = %d, want 0 for an outage shorter than the budget", records)
	}
	if calls, total := tpm.snapshot(); calls != 1 || total != 11 {
		t.Fatalf("TPM calls = %d total = %d, want one", calls, total)
	}
	if usageDBHealth.down() {
		t.Fatal("outage flag raised for a blip the retries absorbed")
	}
}

func TestInsertRequestLogWithoutSpoolKeepsSingleAttempt(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	var attempts atomic.Int32
	writeRequestLogAttemptFunc = func(RequestLogEntry, time.Duration) (requestLogWriteResult, error) {
		attempts.Add(1)
		return requestLogWriteResult{}, io.ErrUnexpectedEOF
	}
	InsertRequestLog(resilienceTestEntry("key-a", "model-a", 10, time.Now()))
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1: without a spool the worker must not stall on retries", got)
	}
}

func TestInsertRequestLogSkipsAnAlreadyStoredKey(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	entry := resilienceTestEntry("key-a", "model-a", 10, time.Now())
	InsertRequestLog(entry)
	InsertRequestLog(entry)
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want 1", n)
	}
	if requests, tokens := lifetimeRollupTotals(t); requests != 1 || tokens != 11 {
		t.Fatalf("lifetime rollup = %d requests / %d tokens, want 1 / 11", requests, tokens)
	}
}

func TestRecordCarriesThePublishedIdempotencyKey(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	stats := NewRequestStatistics()
	record := coreusage.Record{
		IdempotencyKey: coreusage.NewIdempotencyKey(),
		APIKey:         "sk-resilience-XXXX",
		Model:          "model-a",
		RequestedAt:    time.Now(),
		Detail:         coreusage.Detail{InputTokens: 5, OutputTokens: 5},
	}
	stats.Record(context.Background(), record)
	stats.Record(context.Background(), record)
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want the second delivery of one record skipped", n)
	}
}

func TestUsageSpoolReplayIsOrderedAndIdempotent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	base := time.Now().Add(-time.Minute)
	entries := []RequestLogEntry{
		resilienceTestEntry("key-a", "model-a", 10, base),
		resilienceTestEntry("key-b", "model-b", 20, base.Add(time.Second)),
		resilienceTestEntry("key-c", "model-c", 30, base.Add(2*time.Second)),
	}
	spoolAll := func() {
		for _, entry := range entries {
			if !spoolLiveRequestLog(spool, entry, usageSpoolReasonWriteFailed, time.Time{}, nil) {
				t.Fatal("spoolLiveRequestLog() = false")
			}
		}
	}
	spoolAll()
	// The same record spooled a second time must still count once.
	spoolLiveRequestLog(spool, entries[0], usageSpoolReasonWriteFailed, time.Time{}, nil)

	replayer := newUsageSpoolReplayer(spool)
	if err := replayer.drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if got := strings.Join(requestLogModelsInOrder(t), ","); got != "model-a,model-b,model-c" {
		t.Fatalf("replayed order = %s, want model-a,model-b,model-c", got)
	}
	if requests, tokens := lifetimeRollupTotals(t); requests != 3 || tokens != 63 {
		t.Fatalf("lifetime rollup = %d requests / %d tokens, want 3 / 63", requests, tokens)
	}
	if records, bytes, segments := spool.pending(); records != 0 || bytes != 0 || segments != 0 {
		t.Fatalf("spool after drain = %d records / %d bytes / %d segments, want empty", records, bytes, segments)
	}
	if got := spool.replayed.Load(); got != 3 {
		t.Fatalf("replayed = %d, want 3", got)
	}
	if got := spool.duplicates.Load(); got != 1 {
		t.Fatalf("duplicates = %d, want 1", got)
	}

	// Replaying the whole batch again changes nothing.
	spoolAll()
	if err := replayer.drain(); err != nil {
		t.Fatalf("second drain() error = %v", err)
	}
	if n := countRequestLogRows(t); n != 3 {
		t.Fatalf("request_logs rows after a second replay = %d, want 3", n)
	}
	if requests, _ := lifetimeRollupTotals(t); requests != 3 {
		t.Fatalf("lifetime requests after a second replay = %d, want 3", requests)
	}
}

func TestUsageSpoolReplayPausesWhileTheDatabaseIsDown(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	for i, key := range []string{"key-a", "key-b", "key-c"} {
		spoolLiveRequestLog(spool, resilienceTestEntry(key, "model-"+key, int64(i+1), time.Now()), usageSpoolReasonWriteFailed, time.Time{}, nil)
	}
	var down atomic.Bool
	writeRequestLogAttemptFunc = func(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		if entry.IdempotencyKey == "key-b" && down.Load() {
			return requestLogWriteResult{}, errFakeAdminShutdown
		}
		return writeRequestLogAttempt(entry, timeout)
	}
	down.Store(true)
	replayer := newUsageSpoolReplayer(spool)
	if err := replayer.drain(); err == nil {
		t.Fatal("drain() error = nil, want the outage reported")
	}
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("rows while paused = %d, want only the record before the failure", n)
	}
	if !usageDBHealth.down() {
		t.Fatal("outage flag not raised by a failed replay")
	}
	down.Store(false)
	if err := replayer.drain(); err != nil {
		t.Fatalf("drain() after recovery error = %v", err)
	}
	if got := strings.Join(requestLogModelsInOrder(t), ","); got != "model-key-a,model-key-b,model-key-c" {
		t.Fatalf("order after resume = %s", got)
	}
	if usageDBHealth.down() {
		t.Fatal("outage flag still raised after a successful replay")
	}
	if got := spool.duplicates.Load(); got != 0 {
		t.Fatalf("duplicates = %d, want 0: the resume point skips records already stored", got)
	}
}

func TestUsageSpoolResumesAfterRestart(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	dir := filepath.Join(t.TempDir(), "usage-spool")
	first, err := openUsageSpool(dir, 0)
	if err != nil {
		t.Fatalf("openUsageSpool() error = %v", err)
	}
	entries := []RequestLogEntry{
		resilienceTestEntry("key-a", "model-a", 10, time.Now()),
		resilienceTestEntry("key-b", "model-b", 20, time.Now()),
		resilienceTestEntry("key-c", "model-c", 30, time.Now()),
	}
	for _, entry := range entries {
		if !spoolLiveRequestLog(first, entry, usageSpoolReasonWriteFailed, time.Time{}, nil) {
			t.Fatal("spoolLiveRequestLog() = false")
		}
	}
	// key-a was stored just before the process died, its segment not yet deleted.
	if _, outcome, err := writeRequestLogWithRetry(entries[0], 0); outcome != requestLogWriteCommitted {
		t.Fatalf("direct write outcome = %v, err = %v", outcome, err)
	}
	first.close()
	// A crash mid-append leaves a torn line at the end of the newest segment.
	segments, _ := filepath.Glob(filepath.Join(dir, usageSpoolSegmentPrefix+"*"+usageSpoolSegmentSuffix))
	if len(segments) == 0 {
		t.Fatal("no spool segment left on disk")
	}
	f, err := os.OpenFile(segments[len(segments)-1], os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	_, _ = f.WriteString(`{"v":1,"key":"torn`)
	_ = f.Close()

	second := useUsageSpoolForTest(t, dir, 0)
	if records, _, _ := second.pending(); records != 4 {
		t.Fatalf("adopted records = %d, want 3 records plus the torn line", records)
	}
	if err := newUsageSpoolReplayer(second).drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if n := countRequestLogRows(t); n != 3 {
		t.Fatalf("request_logs rows = %d, want 3", n)
	}
	if requests, _ := lifetimeRollupTotals(t); requests != 3 {
		t.Fatalf("lifetime requests = %d, want 3", requests)
	}
	if got := second.duplicates.Load(); got != 1 {
		t.Fatalf("duplicates = %d, want 1 (key-a)", got)
	}
	if got := second.corrupt.Load(); got != 1 {
		t.Fatalf("corrupt = %d, want the torn line skipped", got)
	}
	if records, _, segs := second.pending(); records != 0 || segs != 0 {
		t.Fatalf("spool after restart replay = %d records / %d segments, want empty", records, segs)
	}
}

func TestUsageSpoolDropsTheOldestRecordsWhenFull(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	const maxBytes = 8 << 10
	spool := useUsageSpoolForTest(t, "", maxBytes)
	const total = 80
	for i := 0; i < total; i++ {
		entry := resilienceTestEntry(fmt.Sprintf("key-%02d", i), fmt.Sprintf("model-%02d", i), int64(i), time.Now())
		if !spoolLiveRequestLog(spool, entry, usageSpoolReasonWriteFailed, time.Time{}, nil) {
			t.Fatalf("record %d was not kept", i)
		}
	}
	records, bytes, _ := spool.pending()
	if bytes > maxBytes {
		t.Fatalf("spool holds %d bytes, over its %d byte cap", bytes, maxBytes)
	}
	dropped := spool.droppedRecords.Load()
	if dropped == 0 || records+dropped != total {
		t.Fatalf("pending %d + dropped %d, want some dropped and %d in total", records, dropped, total)
	}
	if err := newUsageSpoolReplayer(spool).drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	models := requestLogModelsInOrder(t)
	if len(models) == 0 || models[len(models)-1] != fmt.Sprintf("model-%02d", total-1) || models[0] == "model-00" {
		t.Fatalf("replayed %v, want the newest kept and the oldest dropped", models)
	}
}

func TestUsageSpoolFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "usage-spool")
	spool, err := openUsageSpool(dir, 0)
	if err != nil {
		t.Fatalf("openUsageSpool() error = %v", err)
	}
	defer spool.close()
	if err := spool.append([]byte("{}\n")); err != nil {
		t.Fatalf("append() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("spool dir mode = %o, want 700", mode)
	}
	segments, _ := filepath.Glob(filepath.Join(dir, "*"+usageSpoolSegmentSuffix))
	if len(segments) != 1 {
		t.Fatalf("segments = %v, want one", segments)
	}
	info, err = os.Stat(segments[0])
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("segment mode = %o, want 600", mode)
	}
}

type usagePluginFunc func(context.Context, coreusage.Record)

func (f usagePluginFunc) HandleUsage(ctx context.Context, record coreusage.Record) { f(ctx, record) }

func TestFullUsageQueueSpoolsInsteadOfBlockingTheRequest(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	tpm := captureTokenUsageForTest(t)
	manager := coreusage.NewManager(1)
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	manager.Register(usagePluginFunc(func(context.Context, coreusage.Record) {
		once.Do(func() { close(started) })
		<-release
	}))
	manager.SetOverflowHandler(overflowUsageRecord)
	record := func(model string) coreusage.Record {
		return coreusage.Record{APIKey: "sk-resilience-XXXX", Model: model, RequestedAt: time.Now(), Detail: coreusage.Detail{InputTokens: 4, OutputTokens: 3}}
	}
	manager.Publish(context.Background(), record("first"))
	<-started
	manager.Publish(context.Background(), record("second"))

	done := make(chan struct{})
	go func() {
		manager.Publish(context.Background(), record("third"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing into a full usage queue blocked the request")
	}
	if records, _, _ := spool.pending(); records != 1 {
		t.Fatalf("spooled records = %d, want the overflowed record", records)
	}
	if calls, total := tpm.snapshot(); calls != 1 || total != 7 {
		t.Fatalf("TPM calls = %d total = %d, want the overflowed record counted at once", calls, total)
	}
	close(release)
	manager.Stop()
	manager.Wait(context.Background())
}

// A retry that finds our own key right after a lost COMMIT reply does not
// trust it until the settle window has passed: under synchronous replication
// that commit may exist only on a primary that is about to be fenced.
func TestLostCommitReplyIsRecheckedBeforeItIsTrusted(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	spool := useUsageSpoolForTest(t, "", 0)
	tpm := captureTokenUsageForTest(t)
	var calls atomic.Int32
	writeRequestLogAttemptFunc = func(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		result, err := writeRequestLogAttempt(entry, timeout)
		if calls.Add(1) == 1 && err == nil {
			// The COMMIT was applied, but the reply never reached us.
			return result, fmt.Errorf("commit log insert: %w", &commitUncertainError{err: io.ErrUnexpectedEOF})
		}
		return result, err
	}

	InsertRequestLog(resilienceTestEntry("key-a", "model-a", 10, time.Now()))
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want 1", n)
	}
	if records, _, _ := spool.pending(); records != 1 {
		t.Fatalf("spooled records = %d, want the unconfirmed record kept for a recheck", records)
	}
	if calls, total := tpm.snapshot(); calls != 1 || total != 11 {
		t.Fatalf("TPM calls = %d total = %d, want one", calls, total)
	}

	writeRequestLogAttemptFunc = writeRequestLogAttempt
	started := time.Now()
	if err := newUsageSpoolReplayer(spool).drain(); err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < requestLogCommitSettleWindow/2 {
		t.Fatalf("recheck after %s, want it to wait for the settle window", elapsed)
	}
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want 1", n)
	}
	if requests, _ := lifetimeRollupTotals(t); requests != 1 {
		t.Fatalf("lifetime requests = %d, want 1", requests)
	}
	if got := spool.duplicates.Load(); got != 1 {
		t.Fatalf("duplicates = %d, want the recheck to confirm the stored row", got)
	}
}

// When failover does discard that commit, the recheck writes the record.
func TestLostCommitDiscardedByFailoverIsWrittenAgain(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetUsageWriteStateForTest(t)
	requestLogCommitSettleWindow = 600 * time.Millisecond
	spool := useUsageSpoolForTest(t, "", 0)
	var calls atomic.Int32
	writeRequestLogAttemptFunc = func(entry RequestLogEntry, timeout time.Duration) (requestLogWriteResult, error) {
		result, err := writeRequestLogAttempt(entry, timeout)
		if calls.Add(1) == 1 && err == nil {
			return result, fmt.Errorf("commit log insert: %w", &commitUncertainError{err: errFakeAdminShutdown})
		}
		return result, err
	}
	InsertRequestLog(resilienceTestEntry("key-a", "model-a", 10, time.Now()))
	writeRequestLogAttemptFunc = writeRequestLogAttempt

	drained := make(chan error, 1)
	go func() { drained <- newUsageSpoolReplayer(spool).drain() }()
	// The promoted standby never received the transaction.
	time.Sleep(150 * time.Millisecond)
	db := getDB()
	for _, stmt := range []string{
		`DELETE FROM request_logs`,
		`DELETE FROM usage_rollup_buckets`,
		`DELETE FROM request_log_idempotency_keys`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := <-drained; err != nil {
		t.Fatalf("drain() error = %v", err)
	}
	if n := countRequestLogRows(t); n != 1 {
		t.Fatalf("request_logs rows = %d, want the discarded record written again", n)
	}
	if requests, tokens := lifetimeRollupTotals(t); requests != 1 || tokens != 11 {
		t.Fatalf("lifetime rollup = %d / %d, want 1 / 11", requests, tokens)
	}
}

func TestUsageSpoolRecordRoundTripKeepsCostInputs(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
	entry := RequestLogEntry{
		IdempotencyKey: "key-a", TrustedTenantID: "tenant-a", APIKey: "sk-resilience-XXXX", APIKeyID: "id-a",
		AuthSubjectID: "subject-a", APIKeyName: "name-a", Model: "model-a", UpstreamModel: "up-a",
		UpstreamResponseModel: "resp-a", VisionFallbackModel: "vision-a", ThinkingLevel: "high",
		Source: "source-a", ChannelName: "channel-a", AuthIndex: "auth-a", Failed: true, Streaming: true,
		Timestamp: time.Date(2026, 9, 24, 8, 0, 0, 123, time.UTC), LatencyMs: 12, FirstTokenMs: 3,
		Tokens: TokenStats{InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3, CachedTokens: 4, TotalTokens: 10,
			CacheReadTokens: 5, CacheWriteTokens: 6, CacheReadIncludedInInput: true},
		InputContent: `{"input":true}`, OutputContent: `{"error":"boom"}`, DetailContent: `{"detail":1}`,
	}
	line, err := encodeUsageSpoolRecord(entry, usageSpoolReasonWriteFailed, time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	decoded, record, err := decodeUsageSpoolRecord(line[:len(line)-1])
	if err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if decoded != entry {
		t.Fatalf("round trip changed the entry\n got: %+v\nwant: %+v", decoded, entry)
	}
	if record.Reason != string(usageSpoolReasonWriteFailed) || !record.CommitUncertainAt.IsZero() {
		t.Fatalf("record metadata = %+v", record)
	}

	// With body storage off the spool keeps no request body on disk.
	SetRequestLogBodyStorageEnabled(false)
	line, err = encodeUsageSpoolRecord(entry, usageSpoolReasonWriteFailed, time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("encode error = %v", err)
	}
	if strings.Contains(string(line), "input_zst") {
		t.Fatalf("spool line keeps the request body with body storage off: %s", line)
	}
	decoded, _, err = decodeUsageSpoolRecord(line[:len(line)-1])
	if err != nil {
		t.Fatalf("decode error = %v", err)
	}
	if decoded.InputContent != "" || decoded.OutputContent == "" || decoded.Tokens != entry.Tokens {
		t.Fatalf("decoded without body storage = %+v", decoded)
	}
}

func TestPruneRequestLogIdempotencyKeysKeepsRecentKeys(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	now := time.Now().UTC()
	for key, at := range map[string]time.Time{
		"old-1": now.Add(-100 * time.Hour),
		"old-2": now.Add(-73 * time.Hour),
		"new-1": now.Add(-time.Hour),
	} {
		if _, err := db.Exec(`INSERT INTO request_log_idempotency_keys (idempotency_key, created_at) VALUES (?, ?)`,
			key, at.Format(requestLogIdempotencyTimeLayout)); err != nil {
			t.Fatalf("insert key: %v", err)
		}
	}
	pruned, err := pruneRequestLogIdempotencyKeys(context.Background(), db, now.Add(-requestLogIdempotencyRetention))
	if err != nil {
		t.Fatalf("prune error = %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	var left string
	if err := db.QueryRow(`SELECT idempotency_key FROM request_log_idempotency_keys`).Scan(&left); err != nil || left != "new-1" {
		t.Fatalf("remaining key = %q (err %v), want new-1", left, err)
	}
}

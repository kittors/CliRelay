package usage

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// usageSpoolRuntime is the running spool and its replayer.
type usageSpoolRuntime struct {
	spool    *usageSpool
	replayer *usageSpoolReplayer
}

var (
	usageSpoolLifecycleMu sync.Mutex
	usageSpoolCurrent     atomic.Pointer[usageSpoolRuntime]
	// usageWriteShuttingDown drops the live retry budget to a single attempt
	// while the usage queue drains at shutdown; records the database cannot
	// take right away go straight to the spool instead of holding up exit.
	usageWriteShuttingDown atomic.Bool
)

// activeUsageSpool returns the running spool, or nil when spooling is off.
// Without a spool the write path keeps its historical behaviour: one attempt
// plus lock-contention retries, and a record the database refuses is dropped.
func activeUsageSpool() *usageSpool {
	if rt := usageSpoolCurrent.Load(); rt != nil {
		return rt.spool
	}
	return nil
}

func liveRequestLogRetryBudget(spool *usageSpool) time.Duration {
	if spool == nil || usageWriteShuttingDown.Load() {
		return 0
	}
	return requestLogLiveRetryBudget
}

// StartUsageSpool opens the request log spool in dir, capped at maxBytes, and
// starts replaying whatever it already holds. From then on a record the
// database cannot take within the retry budget, and a record the full usage
// queue cannot take, is appended to the spool instead of being dropped or
// blocking the request that produced it.
func StartUsageSpool(dir string, maxBytes int64) error {
	usageSpoolLifecycleMu.Lock()
	defer usageSpoolLifecycleMu.Unlock()
	stopUsageSpoolLocked()
	spool, err := openUsageSpool(dir, maxBytes)
	if err != nil {
		return err
	}
	usageWriteShuttingDown.Store(false)
	rt := &usageSpoolRuntime{spool: spool, replayer: newUsageSpoolReplayer(spool)}
	usageSpoolCurrent.Store(rt)
	coreusage.SetOverflowHandler(overflowUsageRecord)
	go rt.replayer.run()
	log.Infof("usage: request log spool ready at %s (cap %d MB)", spool.dir, spool.maxBytes>>20)
	return nil
}

// StopUsageSpool is the shutdown step between the HTTP server draining and
// the database closing. It stops the default usage queue (a no-op once the
// service shutdown has done so) and waits, bounded by ctx, for the records
// still queued to be written or spooled; then it stops the replayer and seals
// the spool. Records still pending stay on disk for the next start.
func StopUsageSpool(ctx context.Context) {
	usageSpoolLifecycleMu.Lock()
	defer usageSpoolLifecycleMu.Unlock()
	if usageSpoolCurrent.Load() == nil {
		return
	}
	usageWriteShuttingDown.Store(true)
	coreusage.StopDefault()
	if !coreusage.WaitDefault(ctx) {
		log.Warnf("usage: shutdown timed out with %d request log records still queued", coreusage.PendingDefault())
	}
	stopUsageSpoolLocked()
}

func stopUsageSpoolLocked() {
	rt := usageSpoolCurrent.Load()
	if rt == nil {
		return
	}
	rt.replayer.close()
	usageSpoolCurrent.Store(nil)
	rt.spool.close()
	if records, bytes, _ := rt.spool.pending(); records > 0 {
		log.Warnf("usage: %d request log records (%d bytes) left in spool %s; they are replayed on the next start", records, bytes, rt.spool.dir)
	}
}

// overflowUsageRecord takes a record the usage queue could not accept. It runs
// on the goroutine that published the record, usually a request that has not
// answered its client yet, so it only touches memory and the local disk.
func overflowUsageRecord(record coreusage.Record) bool {
	spool := activeUsageSpool()
	if spool == nil {
		log.Errorf("usage: request log %s lost: the usage queue cannot take it and no spool is running", record.IdempotencyKey)
		return false
	}
	entry := normalizeRequestLogEntry(defaultRequestStatistics.ingest(context.Background(), record))
	reason := usageSpoolReasonQueueFull
	if usageWriteShuttingDown.Load() {
		reason = usageSpoolReasonShutdown
	}
	return spoolLiveRequestLog(spool, entry, reason, time.Time{}, nil)
}

// spoolLiveRequestLog appends a live record to the spool. TPM is counted here,
// when the record is accepted; the replay will not count it again.
// commitUncertainAt carries a lost COMMIT reply over to the replayer.
func spoolLiveRequestLog(spool *usageSpool, entry RequestLogEntry, reason usageSpoolReason, commitUncertainAt time.Time, cause error) bool {
	line, err := encodeUsageSpoolRecord(entry, reason, commitUncertainAt, time.Now())
	if err == nil {
		err = spool.append(line)
	}
	if err != nil {
		if cause != nil {
			log.Errorf("usage: request log %s lost: the database refused it (%v) and the spool could not keep it: %v", entry.IdempotencyKey, cause, err)
		} else {
			log.Errorf("usage: request log %s lost: the spool could not keep it: %v", entry.IdempotencyKey, err)
		}
		return false
	}
	notifyTokenUsage(entry, "")
	return true
}

// tokenUsageCallback feeds the TPM limiter. endUserID is the account the write
// resolved for the key; it is empty for a record spooled without a database
// round trip, and the limiter then uses the subject it saw at admission.
var tokenUsageCallback func(apiKey, endUserID string, totalTokens int64)

// SetTokenUsageCallback registers a function to be called once for each
// request's tokens: when its log is committed, or when it is spooled. Used by
// the quota middleware for TPM tracking.
func SetTokenUsageCallback(fn func(apiKey, endUserID string, totalTokens int64)) {
	tokenUsageCallback = fn
}

func notifyTokenUsage(entry RequestLogEntry, endUserID string) {
	if tokenUsageCallback != nil && entry.Tokens.TotalTokens > 0 {
		tokenUsageCallback(entry.APIKey, endUserID, entry.Tokens.TotalTokens)
	}
}

// usageDBHealthState is the outage flag of the request log write path. It is
// raised when a record exhausts its retries and cleared by the next write the
// database accepts, so an outage costs one retry budget rather than one per
// record.
type usageDBHealthState struct {
	downSince atomic.Int64
	lastError atomic.Pointer[string]
}

var usageDBHealth usageDBHealthState

func (h *usageDBHealthState) down() bool { return h.downSince.Load() != 0 }

func (h *usageDBHealthState) markDown(err error) {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	h.lastError.Store(&msg)
	if h.downSince.CompareAndSwap(0, time.Now().UnixNano()) {
		log.Warnf("usage: runtime database unavailable for request log writes (%s); records go to the local spool until it accepts writes again", msg)
	}
}

func (h *usageDBHealthState) markUp() {
	if h.downSince.Load() == 0 {
		return
	}
	if since := h.downSince.Swap(0); since != 0 {
		log.Infof("usage: runtime database accepts request log writes again after %s", time.Since(time.Unix(0, since)).Round(time.Second))
	}
}

func (h *usageDBHealthState) snapshot() (time.Time, string) {
	var since time.Time
	if nanos := h.downSince.Load(); nanos != 0 {
		since = time.Unix(0, nanos)
	}
	msg := ""
	if p := h.lastError.Load(); p != nil {
		msg = *p
	}
	return since, msg
}

// UsageWriteStatus is the request log write path as shown on the system
// status endpoint.
type UsageWriteStatus struct {
	// SpoolEnabled is false when spooling is switched off or failed to start.
	SpoolEnabled bool   `json:"spool_enabled"`
	SpoolDir     string `json:"spool_dir,omitempty"`
	// DatabaseAvailable is false while an outage is known to the write path.
	DatabaseAvailable bool       `json:"database_available"`
	UnavailableSince  *time.Time `json:"unavailable_since,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	QueueDepth        int        `json:"queue_depth"`
	PendingRecords    int64      `json:"pending_records"`
	PendingBytes      int64      `json:"pending_bytes"`
	Replaying         bool       `json:"replaying"`
	SpooledTotal      int64      `json:"spooled_total"`
	ReplayedTotal     int64      `json:"replayed_total"`
	DuplicateTotal    int64      `json:"duplicate_total"`
	DroppedTotal      int64      `json:"dropped_total"`
	RejectedTotal     int64      `json:"rejected_total"`
}

// GetUsageWriteStatus reports spool backlog and outage state.
func GetUsageWriteStatus() UsageWriteStatus {
	status := UsageWriteStatus{DatabaseAvailable: !usageDBHealth.down(), QueueDepth: coreusage.PendingDefault()}
	if since, msg := usageDBHealth.snapshot(); !since.IsZero() {
		status.UnavailableSince = &since
		status.LastError = msg
	}
	rt := usageSpoolCurrent.Load()
	if rt == nil {
		return status
	}
	s := rt.spool
	status.SpoolEnabled = true
	status.SpoolDir = s.dir
	status.PendingRecords, status.PendingBytes, _ = s.pending()
	status.Replaying = rt.replayer.replaying.Load()
	status.SpooledTotal = s.appended.Load()
	status.ReplayedTotal = s.replayed.Load()
	status.DuplicateTotal = s.duplicates.Load()
	status.DroppedTotal = s.droppedRecords.Load() + s.corrupt.Load()
	status.RejectedTotal = s.rejected.Load()
	return status
}

// UsageSpoolMaxBytes converts the configured size in MB to bytes. Zero means
// the default; a negative value disables the spool.
func UsageSpoolMaxBytes(maxSizeMB int) (int64, bool) {
	switch {
	case maxSizeMB < 0:
		return 0, false
	case maxSizeMB == 0:
		return usageSpoolDefaultMaxBytes, true
	default:
		return int64(maxSizeMB) << 20, true
	}
}

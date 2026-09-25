package usage

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Replay pacing; variables only so tests can shorten them.
var (
	usageSpoolReplayMinBackoff = time.Second
	// usageSpoolReplayMaxBackoff caps the probe interval during an outage, so
	// replay starts within seconds of a failover completing.
	usageSpoolReplayMaxBackoff = 5 * time.Second
	// usageSpoolIdlePoll re-checks the spool when no append woke the replayer.
	usageSpoolIdlePoll = 30 * time.Second
)

// usageSpoolBacklogLogInterval rate-limits the "records waiting" warning.
const usageSpoolBacklogLogInterval = 30 * time.Second

var errUsageSpoolReplayStopped = errors.New("usage: spool replay stopped")

// usageSpoolReplayer writes spooled records back in the order they were
// spooled, one at a time. It never counts TPM: that was counted when the
// record was accepted, and replaying minutes later would throttle current
// traffic with tokens that belong to a window long gone.
type usageSpoolReplayer struct {
	spool *usageSpool
	stop  chan struct{}
	done  chan struct{}

	replaying atomic.Bool
	// resumeSeq/resumeOffset remember how far into a segment the last pass got,
	// so a pass interrupted by the database going away again does not resend
	// the records it already stored. Idempotency keys make resending safe, this
	// only saves the round trips.
	resumeSeq    uint64
	resumeOffset int64
	// uncertainKey/uncertainAt remember a lost COMMIT reply of the record at
	// the head of the spool across passes, so a duplicate found after the
	// database comes back is re-checked instead of trusted.
	uncertainKey string
	uncertainAt  time.Time

	lastBacklogLog time.Time
}

func newUsageSpoolReplayer(spool *usageSpool) *usageSpoolReplayer {
	return &usageSpoolReplayer{spool: spool, stop: make(chan struct{}), done: make(chan struct{})}
}

func (r *usageSpoolReplayer) run() {
	defer close(r.done)
	backoff := usageSpoolReplayMinBackoff
	for {
		err := r.drain()
		if errors.Is(err, errUsageSpoolReplayStopped) {
			return
		}
		healthy := err == nil
		if healthy && usageDBHealth.down() {
			// The spool is empty but the outage flag is still up (the records that
			// raised it may have been dropped for space): probe directly.
			healthy = probeUsageDBWritable()
			if healthy {
				usageDBHealth.markUp()
			}
		}
		if !healthy {
			if err != nil {
				log.Debugf("usage: spool replay paused: %v", err)
			}
			r.logBacklog()
			// Appends keep arriving during an outage; ignore their wake-ups and
			// probe on the backoff instead of once per append.
			if !r.sleep(backoff, false) {
				return
			}
			backoff = min(backoff*2, usageSpoolReplayMaxBackoff)
			continue
		}
		backoff = usageSpoolReplayMinBackoff
		if !r.sleep(usageSpoolIdlePoll, true) {
			return
		}
	}
}

// sleep waits for d, the stop signal, or (when wakeOnAppend) a new record. It
// returns false when the replayer should exit.
func (r *usageSpoolReplayer) sleep(d time.Duration, wakeOnAppend bool) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	var wake <-chan struct{}
	if wakeOnAppend {
		wake = r.spool.wake
	}
	select {
	case <-r.stop:
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

func (r *usageSpoolReplayer) stopping() bool {
	select {
	case <-r.stop:
		return true
	default:
		return false
	}
}

// drain replays segments until the spool is empty, the database fails again,
// or the replayer is stopped.
func (r *usageSpoolReplayer) drain() error {
	for {
		if r.stopping() {
			return errUsageSpoolReplayStopped
		}
		seg, ok := r.spool.leaseOldest()
		if !ok {
			return nil
		}
		if err := r.replaySegment(seg); err != nil {
			r.spool.releaseLease()
			return err
		}
	}
}

func (r *usageSpoolReplayer) replaySegment(seg usageSpoolSegment) error {
	offset := int64(0)
	if seg.seq == r.resumeSeq {
		offset = r.resumeOffset
	}
	r.replaying.Store(true)
	defer r.replaying.Store(false)

	var stopErr error
	readErr := readUsageSpoolSegment(seg.path, offset, func(line []byte, complete bool, next int64) bool {
		if r.stopping() {
			stopErr = errUsageSpoolReplayStopped
			return false
		}
		if err := r.replayLine(seg, line, complete); err != nil {
			stopErr = err
			return false
		}
		offset = next
		return true
	})
	r.resumeSeq, r.resumeOffset = seg.seq, offset
	if stopErr != nil {
		return stopErr
	}
	if readErr != nil {
		return readErr
	}
	r.resumeSeq, r.resumeOffset = 0, 0
	return r.spool.completeLeased(seg.seq)
}

// replayLine writes one spooled record. It returns an error only when the
// database is unreachable, which pauses the replay at this record.
func (r *usageSpoolReplayer) replayLine(seg usageSpoolSegment, line []byte, complete bool) error {
	if !complete {
		// A crash mid-append leaves a partial last line; there is nothing to recover.
		r.spool.corrupt.Add(1)
		r.spool.recordDone(seg.seq)
		log.Warnf("usage: skipped a truncated record at the end of spool segment %s", seg.path)
		return nil
	}
	entry, record, err := decodeUsageSpoolRecord(line)
	if err != nil {
		r.spool.corrupt.Add(1)
		r.spool.recordDone(seg.seq)
		log.Errorf("usage: skipped an unreadable record in spool segment %s: %v", seg.path, err)
		return nil
	}
	if age := time.Since(record.SpooledAt); age > requestLogIdempotencyRetention {
		log.Warnf("usage: replaying request log %s spooled %s ago, past the %s duplicate-detection window; if its first attempt had in fact committed it is now counted twice",
			record.Key, age.Round(time.Minute), requestLogIdempotencyRetention)
	}
	uncertainAt := record.CommitUncertainAt
	if r.uncertainKey == record.Key && r.uncertainAt.After(uncertainAt) {
		uncertainAt = r.uncertainAt
	}
	for {
		result, outcome, err := writeRequestLogWithRetry(entry, 0)
		if result.commitUncertainAt.After(uncertainAt) {
			uncertainAt = result.commitUncertainAt
			r.uncertainKey, r.uncertainAt = record.Key, uncertainAt
		}
		result.commitUncertainAt = uncertainAt
		switch outcome {
		case requestLogWriteCommitted:
			usageDBHealth.markUp()
			if result.duplicateNeedsRecheck(time.Now()) {
				wait := time.Until(uncertainAt.Add(requestLogCommitSettleWindow))
				log.Infof("usage: spooled request log %s is already stored, but by a commit whose reply was lost; re-checking in %s in case a failover discards it",
					record.Key, wait.Round(time.Second))
				if !r.sleep(wait, false) {
					return errUsageSpoolReplayStopped
				}
				continue
			}
			r.uncertainKey, r.uncertainAt = "", time.Time{}
			if result.duplicate {
				r.spool.duplicates.Add(1)
			} else {
				r.spool.replayed.Add(1)
			}
			r.spool.recordDone(seg.seq)
			return nil
		case requestLogWriteTransient:
			usageDBHealth.markDown(err)
			return err
		default:
			// The database rejected the row itself; it would fail the same way
			// forever, and the live path drops such rows too.
			r.uncertainKey, r.uncertainAt = "", time.Time{}
			r.spool.rejected.Add(1)
			r.spool.recordDone(seg.seq)
			log.Errorf("usage: dropped spooled request log %s (spooled %s): the database rejected it: %v",
				record.Key, record.SpooledAt.Format(time.RFC3339), err)
			return nil
		}
	}
}

func (r *usageSpoolReplayer) logBacklog() {
	records, bytes, _ := r.spool.pending()
	if records == 0 || time.Since(r.lastBacklogLog) < usageSpoolBacklogLogInterval {
		return
	}
	r.lastBacklogLog = time.Now()
	since, lastErr := usageDBHealth.snapshot()
	log.Warnf("usage: %d request log records (%d bytes) waiting in spool %s; database unavailable since %s: %s",
		records, bytes, r.spool.dir, since.Format(time.RFC3339), lastErr)
}

func (r *usageSpoolReplayer) close() {
	close(r.stop)
	<-r.done
}

// probeUsageDBWritable reports whether the runtime database accepts writes.
// On PostgreSQL a successful ping is not enough: a pooled connection can still
// reach the demoted former primary, which answers reads but refuses writes.
func probeUsageDBWritable() bool {
	db := getDB()
	if db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestLogWriteAttemptTimeout)
	defer cancel()
	if currentUsageDriver() != "postgres" {
		return db.PingContext(ctx) == nil
	}
	var inRecovery bool
	if err := db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return false
	}
	return !inRecovery
}

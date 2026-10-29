package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// requestLogSessionBackfillMarker records that request_logs.session_id has been
// filled from request_log_content for the rows written before the column
// existed. Once "done" the pass never runs again.
const (
	requestLogSessionBackfillMarker = "request_logs_session_id_backfill_v1"
	requestLogSessionBackfillDone   = "done"
)

// The knobs follow the request_logs.api_key masking pass (see
// requestLogKeyMaskStartDelay and its siblings), which runs on the same table
// under the same constraints. They are variables only so tests can run the pass
// without waiting.
var (
	// requestLogSessionBackfillStartDelay waits out the previous build: during a
	// blue-green hand-over it keeps writing rows without session_id until its
	// drain window ends.
	requestLogSessionBackfillStartDelay = 10 * time.Minute
	// requestLogSessionBackfillWindow is the span of request_logs ids one UPDATE
	// covers. Walking the primary key in windows bounds the rows, locks and WAL
	// of every statement without needing an index on session_id.
	requestLogSessionBackfillWindow int64 = 2000
	// requestLogSessionBackfillBatchPause is the sleep between two windows.
	requestLogSessionBackfillBatchPause = 200 * time.Millisecond
	// requestLogSessionBackfillBatchTimeout caps one statement.
	requestLogSessionBackfillBatchTimeout = 60 * time.Second
	// requestLogSessionBackfillRetryPause is the wait after a failed pass, and
	// between leadership checks on a node that is not the leader.
	requestLogSessionBackfillRetryPause = 60 * time.Second
	// requestLogSessionBackfillSettlePause separates the verification pass from
	// the one before it, so a straggling writer of the previous build is seen.
	requestLogSessionBackfillSettlePause = 30 * time.Second
)

var requestLogSessionBackfillScheduled atomic.Bool

// requestLogSessionBackfillSleep is the wait of the scheduler loop; a variable
// so tests can observe the waits without sitting through them.
var requestLogSessionBackfillSleep = time.Sleep

// ScheduleRequestLogSessionBackfill copies request_log_content.session_id onto
// the request_logs rows written before request_logs had a session_id column, in
// the background.
//
// Only rows that still have their content row can be filled: content is kept for
// content-retention-days, so the work is bounded by that window rather than by
// the size of request_logs, and older rows keep an empty session_id, which is
// what session counts already saw for them. Like the key masking pass it waits
// requestLogSessionBackfillStartDelay, runs only on the cluster leader, walks
// the table in small windows with a pause between them, never overwrites a
// session_id that is already set, and ends with a verification pass before
// writing its marker.
func ScheduleRequestLogSessionBackfill() {
	if !requestLogSessionBackfillScheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer requestLogSessionBackfillScheduled.Store(false)
		if d := requestLogSessionBackfillStartDelay; d > 0 {
			requestLogSessionBackfillSleep(d)
		}
		for attempt := 1; ; attempt++ {
			db := getDB()
			if db == nil {
				// The usage store was closed (shutdown).
				return
			}
			if requestLogSessionBackfillCompleted(db) {
				return
			}
			if !cluster.Default().IsLeader() {
				requestLogSessionBackfillSleep(requestLogSessionBackfillRetryPause)
				continue
			}
			if err := runRequestLogSessionBackfill(context.Background(), db); err != nil {
				log.Warnf("usage: request_logs.session_id backfill attempt %d failed: %v", attempt, err)
				requestLogSessionBackfillSleep(requestLogSessionBackfillRetryPause)
				continue
			}
			return
		}
	}()
}

func requestLogSessionBackfillCompleted(db *sql.DB) bool {
	return projectionMarkerValue(db, requestLogSessionBackfillMarker) == requestLogSessionBackfillDone
}

// runRequestLogSessionBackfill fills until a verification pass finds nothing
// left, then writes the done marker.
func runRequestLogSessionBackfill(ctx context.Context, db *sql.DB) error {
	ensureUsageProjectionMarkerTable(db)
	if requestLogSessionBackfillCompleted(db) {
		return nil
	}
	started := time.Now()
	var total int64
	for {
		filled, err := backfillRequestLogSessionsOnce(ctx, db)
		total += filled
		if err != nil {
			return err
		}
		if filled == 0 {
			break
		}
		if err := sleepContext(ctx, requestLogSessionBackfillSettlePause); err != nil {
			return err
		}
	}
	if err := setProjectionMarker(db, requestLogSessionBackfillMarker, requestLogSessionBackfillDone); err != nil {
		return fmt.Errorf("usage: record request_logs.session_id backfill done: %w", err)
	}
	log.Infof("usage: request_logs.session_id backfill finished: %d rows in %s", total, time.Since(started).Round(time.Second))
	return nil
}

// backfillRequestLogSessionsOnce makes one sweep over the id range that still
// has content rows carrying a session id, and returns how many rows it filled.
func backfillRequestLogSessionsOnce(ctx context.Context, db *sql.DB) (int64, error) {
	var first, last sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MIN(log_id), MAX(log_id) FROM request_log_content WHERE session_id <> ''`,
	).Scan(&first, &last); err != nil {
		return 0, fmt.Errorf("usage: session backfill range: %w", err)
	}
	if !first.Valid || !last.Valid {
		return 0, nil
	}
	var total int64
	for start := first.Int64; start <= last.Int64; start += requestLogSessionBackfillWindow {
		n, err := backfillRequestLogSessionWindow(ctx, db, start, start+requestLogSessionBackfillWindow)
		total += n
		if err != nil {
			return total, err
		}
		if err := sleepContext(ctx, requestLogSessionBackfillBatchPause); err != nil {
			return total, err
		}
	}
	return total, nil
}

// backfillRequestLogSessionWindow fills the rows with from <= id < to.
func backfillRequestLogSessionWindow(ctx context.Context, db *sql.DB, from, to int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestLogSessionBackfillBatchTimeout)
	defer cancel()
	// A content row belongs to the log with its id in the same tenant. The
	// correlated form runs on both SQLite and PostgreSQL.
	result, err := db.ExecContext(ctx, `
		UPDATE request_logs
		SET session_id = (
			SELECT content.session_id FROM request_log_content content
			WHERE content.tenant_id = request_logs.tenant_id AND content.log_id = request_logs.id
		)
		WHERE id >= ? AND id < ?
		  AND session_id = ''
		  AND EXISTS (
			SELECT 1 FROM request_log_content content
			WHERE content.tenant_id = request_logs.tenant_id AND content.log_id = request_logs.id
			  AND content.session_id <> ''
		  )`, from, to)
	if err != nil {
		return 0, fmt.Errorf("usage: session backfill window [%d, %d): %w", from, to, err)
	}
	return rowsAffected(result)
}

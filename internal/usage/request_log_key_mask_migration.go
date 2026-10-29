package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// requestLogKeyMaskMarker records that every request_logs.api_key has been
// replaced by its mask. Once "done" the pass never runs again.
const (
	requestLogKeyMaskMarker = "request_logs_api_key_masked_v1"
	requestLogKeyMaskDone   = "done"
)

// The masking pass is a background job that must never compete with traffic or
// with startup, so every knob errs on the gentle side. They are variables only
// so tests can run it without waiting.
var (
	// requestLogKeyMaskStartDelay keeps the pass off until the previous build has
	// stopped writing. A blue-green hand-over leaves the old slot serving for up
	// to its drain window (DRAIN_SECONDS 180 + CLIRELAY_SHUTDOWN_GRACE 300, counted
	// from the cut-over, which itself follows a readiness wait of up to ~90s), and
	// that build still writes the secret into request_logs.api_key. The first pass
	// therefore starts 10 minutes after this process did; stragglers are picked up
	// by the extra passes below.
	requestLogKeyMaskStartDelay = 10 * time.Minute
	// requestLogKeyMaskBatchSize bounds one UPDATE, and with it the row locks and
	// the WAL a single statement produces (every row rewrites its index entries).
	requestLogKeyMaskBatchSize = 500
	// requestLogKeyMaskBatchPause is the sleep between two batches.
	requestLogKeyMaskBatchPause = 400 * time.Millisecond
	// requestLogKeyMaskBatchTimeout caps one statement so a stuck one cannot hold
	// its row locks indefinitely.
	requestLogKeyMaskBatchTimeout = 60 * time.Second
	// requestLogKeyMaskRetryPause is the wait after a failed pass, and between
	// leadership checks on a node that is not the leader.
	requestLogKeyMaskRetryPause = 60 * time.Second
	// requestLogKeyMaskSettlePause separates the final verification pass from the
	// one before it, so a straggling writer of the previous build is seen.
	requestLogKeyMaskSettlePause = 30 * time.Second
)

var requestLogKeyMaskScheduled atomic.Bool

// requestLogKeyMaskSleep waits out the start delay and the retry pauses of the
// scheduler loop. It is a variable so tests can observe the waits without sitting
// through them.
var requestLogKeyMaskSleep = time.Sleep

// ScheduleRequestLogAPIKeyMasking replaces the client secrets that older builds
// stored in request_logs.api_key with their masks, in the background.
//
// What it does for a stored value V that is neither a mask nor a route/origin
// label: resolve V to a key id through api_keys, fill api_key_id on the rows that
// lack one (never overwriting one), and set api_key to MaskAPIKey(V). A value
// that resolves to no key (a deleted key) is masked without an id and keeps its
// api_key_name snapshot. The masking is irreversible by design.
//
// Properties that make it safe to run on a live system:
//   - it waits requestLogKeyMaskStartDelay and never blocks startup or /readyz;
//   - only the cluster leader runs it, and a node that is not the leader keeps
//     checking in case leadership moves here before the marker is written;
//   - rows are updated in small batches with a pause between them, each under a
//     timeout, so locks are short and traffic is not starved;
//   - the state is in the data: a row still holding a secret is work left, so an
//     interrupted pass resumes by simply running again, and a finished one finds
//     nothing; rows that are already masked are never touched;
//   - it ends with a verification pass, then writes the done marker.
func ScheduleRequestLogAPIKeyMasking() {
	if !requestLogKeyMaskScheduled.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer requestLogKeyMaskScheduled.Store(false)
		if d := requestLogKeyMaskStartDelay; d > 0 {
			requestLogKeyMaskSleep(d)
		}
		for attempt := 1; ; attempt++ {
			db := getDB()
			if db == nil {
				// The usage store was closed (shutdown): nothing left to mask here.
				return
			}
			if requestLogKeyMaskCompleted(db) {
				return
			}
			if !cluster.Default().IsLeader() {
				requestLogKeyMaskSleep(requestLogKeyMaskRetryPause)
				continue
			}
			if err := runRequestLogAPIKeyMasking(context.Background(), db); err != nil {
				log.Warnf("usage: masking request_logs.api_key attempt %d failed: %v", attempt, err)
				requestLogKeyMaskSleep(requestLogKeyMaskRetryPause)
				continue
			}
			return
		}
	}()
}

func requestLogKeyMaskCompleted(db *sql.DB) bool {
	return projectionMarkerValue(db, requestLogKeyMaskMarker) == requestLogKeyMaskDone
}

// runRequestLogAPIKeyMasking masks until a verification pass finds nothing left,
// then writes the done marker. It returns early with ctx.Err() when cancelled.
func runRequestLogAPIKeyMasking(ctx context.Context, db *sql.DB) error {
	ensureUsageProjectionMarkerTable(db)
	if requestLogKeyMaskCompleted(db) {
		return nil
	}
	started := time.Now()
	var total int64
	for pass := 1; ; pass++ {
		masked, err := maskRequestLogAPIKeysOnce(ctx, db)
		total += masked
		if err != nil {
			return err
		}
		if masked == 0 {
			break
		}
		// Something was still being written in the clear: give the writer time to
		// stop, then look again.
		if err := sleepContext(ctx, requestLogKeyMaskSettlePause); err != nil {
			return err
		}
	}
	if err := setProjectionMarker(db, requestLogKeyMaskMarker, requestLogKeyMaskDone); err != nil {
		return fmt.Errorf("usage: record request_logs.api_key masking done: %w", err)
	}
	log.Infof("usage: request_logs.api_key masking finished: %d rows in %s", total, time.Since(started).Round(time.Second))
	return nil
}

// maskRequestLogAPIKeysOnce makes one sweep over the distinct stored values and
// returns how many rows it rewrote.
func maskRequestLogAPIKeysOnce(ctx context.Context, db *sql.DB) (int64, error) {
	values, err := distinctRequestLogAPIKeyValues(ctx, db)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, value := range values {
		if IsMaskedAPIKey(value) {
			continue
		}
		n, err := maskRequestLogAPIKeyValue(ctx, db, value)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func distinctRequestLogAPIKeyValues(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT api_key FROM request_logs WHERE api_key <> ''`)
	if err != nil {
		return nil, fmt.Errorf("usage: list stored request_logs.api_key values: %w", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("usage: scan stored request_logs.api_key value: %w", err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// maskRequestLogAPIKeyValue rewrites every row that stores value, in batches.
func maskRequestLogAPIKeyValue(ctx context.Context, db *sql.DB, value string) (int64, error) {
	var keyID string
	switch err := db.QueryRowContext(ctx, `SELECT id FROM api_keys WHERE key = ?`, value).Scan(&keyID); {
	case err == sql.ErrNoRows:
		keyID = ""
	case err != nil:
		return 0, fmt.Errorf("usage: resolve stored api key: %w", err)
	}
	keyID = strings.TrimSpace(keyID)
	// A route or internal origin ("POST /v1/models", "warmup") is not a secret and
	// stays readable. A stored key wins over that shape, though: a secret that
	// happens to read like a label is still a secret.
	if keyID == "" && IsRequestLogLabel(value) {
		return 0, nil
	}
	mask := MaskAPIKey(value)

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := maskRequestLogAPIKeyBatch(ctx, db, value, mask, keyID)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			if total > 0 {
				log.Infof("usage: masked %d request_logs rows of key %s", total, mask)
			}
			return total, nil
		}
		if err := sleepContext(ctx, requestLogKeyMaskBatchPause); err != nil {
			return total, err
		}
	}
}

func maskRequestLogAPIKeyBatch(ctx context.Context, db *sql.DB, value, mask, keyID string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestLogKeyMaskBatchTimeout)
	defer cancel()
	// api_key_id is filled only where it is empty: a row that already names its
	// key keeps that attribution, whatever the secret has been rotated to since.
	//
	// The subquery deliberately has no ORDER BY: the api_key index hands back any
	// batch-size matching rows and stops, whereas ordering by id would make the
	// database visit every remaining row of the key for each batch.
	result, err := db.ExecContext(ctx, `
		UPDATE request_logs
		SET api_key = ?,
		    api_key_id = CASE WHEN trim(coalesce(api_key_id, '')) = '' THEN ? ELSE api_key_id END
		WHERE id IN (
			SELECT id FROM request_logs WHERE api_key = ? LIMIT ?
		)`, mask, keyID, value, requestLogKeyMaskBatchSize)
	if err != nil {
		return 0, fmt.Errorf("usage: mask request_logs.api_key batch: %w", err)
	}
	return rowsAffected(result)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

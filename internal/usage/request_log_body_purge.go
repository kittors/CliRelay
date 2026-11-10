package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// requestBodyPurgeBatchSize is written into the SQL as a constant rather than
// bound: PostgreSQL plans a bound LIMIT as a tenth of the table, which can tip
// the first purge after an upgrade, when every row is in the index, onto a
// sequential scan and a sort.
const requestBodyPurgeBatchSize = 25

// requestLogLegacyBodiesPurgedMarker records that request_logs.input_content and
// output_content were cleared with body storage off. Nothing writes those
// columns any more: the INSERT in insertRequestLogRowTx leaves them out, and
// every other statement only empties them (migrateLegacyContentBatch moves what
// an early build left there into request_log_content). Once cleared they stay
// empty, so the purge stops scanning request_logs for them.
const (
	requestLogLegacyBodiesPurgedMarker = "request_logs_legacy_bodies_purged_v1"
	requestLogLegacyBodiesPurgedDone   = "done"
)

var requestLogBodyPurgeMu sync.Mutex

type RequestLogBodyPurgeResult struct {
	ClearRequestLogsResult
	// CheckedRows counts the request_log_content rows the purge read: rows
	// written while body storage was on, or before bodies_stripped existed. A
	// purge with storage off since the last one reads none.
	CheckedRows             int64 `json:"checked_rows"`
	SanitizedDetailRows     int64 `json:"sanitized_detail_rows"`
	RemovedDetailBytes      int64 `json:"removed_detail_bytes"`
	ReclaimedStorage        bool  `json:"reclaimed_storage"`
	PhysicalReclaimDeferred bool  `json:"physical_reclaim_deferred"`
}

// unstrippedContentRow is what the purge needs to strip one row. The body
// columns are read as lengths only: they are emptied, never rewritten.
type unstrippedContentRow struct {
	LogID       int64
	Compression string
	Detail      []byte
	InputLen    int64
	OutputLen   int64
	SessionLen  int64
	Failed      bool
}

// PurgeStoredRequestBodies removes direct request/response bodies and strips
// bodies embedded in historical request details. It deliberately avoids blocking
// physical rewrite operations such as PostgreSQL VACUUM FULL on the live API path.
func PurgeStoredRequestBodies() (RequestLogBodyPurgeResult, error) {
	usageDBMu.Lock()
	db := usageDB
	driver := usageDriver
	usageDBMu.Unlock()
	if db == nil {
		return RequestLogBodyPurgeResult{}, fmt.Errorf("usage: database not initialised")
	}
	// Turning body storage off from the panel asks for the purge, so it runs
	// to the end rather than stopping with the request.
	return purgeStoredRequestBodies(context.Background(), db, driver)
}

// purgeStoredRequestBodies strips the bodies from the rows that may still hold
// them, in every tenant: the body storage toggle is one setting for the whole
// deployment, and insertLogContentTenantTx strips every tenant's rows alike.
//
// It used to re-read every stored detail on each maintenance pass. On
// production on 2026-11-10 that was 76k rows and 4 to 22 minutes a pass, and
// each pass changed nothing: rows written with storage off are stripped on the
// way in. Each row now records that in bodies_stripped, and the purge reads only
// the rows with 0 through a partial index that is otherwise empty.
//
// ctx stops it between statements and cancels the one in flight: the
// maintenance worker passes its own context, and CloseDB waits for that worker
// before the process can exit. The first purge after an upgrade, or after
// storage was on for a while, still reads many rows. Each batch commits on its
// own, so a stopped purge loses nothing and the next one goes on with the rows
// left.
func purgeStoredRequestBodies(ctx context.Context, db *sql.DB, _ string) (RequestLogBodyPurgeResult, error) {
	requestLogBodyPurgeMu.Lock()
	defer requestLogBodyPurgeMu.Unlock()

	if db == nil {
		return RequestLogBodyPurgeResult{}, fmt.Errorf("usage: database not initialised")
	}

	var result RequestLogBodyPurgeResult
	if RequestLogBodyStorageEnabled() {
		// Storage was turned back on after this purge was asked for. The legacy
		// columns below are moved, not cleared, while it is on.
		log.Info("usage: request log body purge skipped: body storage is on")
		return result, nil
	}
	var err error
	if result.ClearedLegacyRows, err = purgeLegacyRequestLogBodies(ctx, db); err != nil {
		return result, err
	}
	if err = purgeUnstrippedRequestLogContent(ctx, db, &result); err != nil {
		return result, err
	}

	changed := result.ClearedBodyRows > 0 || result.ClearedLegacyRows > 0 || result.SanitizedDetailRows > 0 || result.DeletedContents > 0
	if changed {
		refreshRequestLogContentBytes(db)
		triggerRequestLogCompaction()
	}
	result.PhysicalReclaimDeferred = changed

	log.Infof(
		"usage: purged request log bodies (checked_rows=%d body_rows=%d legacy_rows=%d sanitized_detail_rows=%d deleted_rows=%d removed_detail_bytes=%d physical_reclaim_deferred=%t)",
		result.CheckedRows,
		result.ClearedBodyRows,
		result.ClearedLegacyRows,
		result.SanitizedDetailRows,
		result.DeletedContents,
		result.RemovedDetailBytes,
		result.PhysicalReclaimDeferred,
	)
	return result, nil
}

// purgeLegacyRequestLogBodies clears the bodies an early build stored on
// request_logs itself, once; see requestLogLegacyBodiesPurgedMarker. Failed
// requests keep output_content, as in request_log_content: the error modal
// reads the compact upstream error there.
func purgeLegacyRequestLogBodies(ctx context.Context, db *sql.DB) (int64, error) {
	if projectionMarkerValue(db, requestLogLegacyBodiesPurgedMarker) == requestLogLegacyBodiesPurgedDone {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("usage: begin legacy request log body purge: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	cleared, err := tx.ExecContext(ctx,
		`UPDATE request_logs
		 SET input_content = '',
		     output_content = CASE WHEN failed = 1 THEN output_content ELSE '' END
		 WHERE length(input_content) > 0 OR (failed <> 1 AND length(output_content) > 0)`,
	)
	if err != nil {
		return 0, fmt.Errorf("usage: clear legacy request log bodies: %w", err)
	}
	clearedRows, err := rowsAffected(cleared)
	if err != nil {
		return 0, fmt.Errorf("usage: legacy affected rows: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(marker_key) DO UPDATE SET
		   marker_value = excluded.marker_value,
		   updated_at = excluded.updated_at`,
		requestLogLegacyBodiesPurgedMarker,
		requestLogLegacyBodiesPurgedDone,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, fmt.Errorf("usage: record legacy request log body purge: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("usage: commit legacy request log body purge: %w", err)
	}
	committed = true
	return clearedRows, nil
}

// purgeUnstrippedRequestLogContent walks the rows with bodies_stripped = 0 in
// log_id order, strips each one and marks it stripped.
func purgeUnstrippedRequestLogContent(ctx context.Context, db *sql.DB, result *RequestLogBodyPurgeResult) error {
	var lastLogID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Rows written from here on keep their bodies on purpose; the ones not
		// reached yet are the next purge's once storage is off again.
		if RequestLogBodyStorageEnabled() {
			log.Info("usage: request log body purge stopped: body storage was turned back on")
			return nil
		}
		ids, err := nextUnstrippedRequestLogIDs(ctx, db, lastLogID)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		lastLogID = ids[len(ids)-1]
		if err = stripRequestLogContentBatch(ctx, db, ids, result); err != nil {
			return err
		}
	}
}

// unstrippedRequestLogIDsSQL reads log_id alone, which SQLite answers from the
// partial index without touching the table; given any other column it prefers
// a rowid range scan that reads every row past the cursor to test the flag.
// bodies_stripped = 0 stays a literal for the same reason on both drivers: a
// bound value cannot be matched to the index predicate. The cursor needs no
// tenant because log ids come from one sequence (see oldestContentRowsForTrim).
var unstrippedRequestLogIDsSQL = `SELECT log_id FROM request_log_content
	 WHERE bodies_stripped = 0 AND log_id > ?
	 ORDER BY log_id ASC
	 LIMIT ` + strconv.Itoa(requestBodyPurgeBatchSize)

// unstrippedContentRowsSQL loads the rows of one batch by log_id; see
// unstrippedRequestLogIDsSQL for the literal flag.
func unstrippedContentRowsSQL(rows int) string {
	return `SELECT c.log_id, c.compression, c.detail_content,
	        length(c.input_content), length(c.output_content), length(c.session_id),
	        COALESCE(l.failed, 0)
	 FROM request_log_content c
	 LEFT JOIN request_logs l ON l.tenant_id = c.tenant_id AND l.id = c.log_id
	 WHERE c.bodies_stripped = 0 AND c.log_id IN (` + strings.TrimSuffix(strings.Repeat("?,", rows), ",") + `)
	 ORDER BY c.log_id ASC`
}

func nextUnstrippedRequestLogIDs(ctx context.Context, db *sql.DB, afterLogID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, unstrippedRequestLogIDsSQL, afterLogID)
	if err != nil {
		return nil, fmt.Errorf("usage: query unstripped request log content: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, requestBodyPurgeBatchSize)
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("usage: scan unstripped request log content: %w", err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate unstripped request log content: %w", err)
	}
	return ids, nil
}

func stripRequestLogContentBatch(ctx context.Context, db *sql.DB, ids []int64, result *RequestLogBodyPurgeResult) error {
	batch, err := loadUnstrippedContentRows(ctx, db, ids)
	if err != nil {
		return err
	}
	return applyContentStrip(ctx, db, batch, result)
}

// applyContentStrip strips the rows of one batch as they were read, in one
// transaction.
func applyContentStrip(ctx context.Context, db *sql.DB, batch []unstrippedContentRow, result *RequestLogBodyPurgeResult) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usage: begin request log body purge batch: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, row := range batch {
		result.CheckedRows++
		plan, errPlan := planContentStrip(row)
		if errPlan != nil {
			return errPlan
		}
		// A flag of 1 is the only thing that keeps a row out of later purges,
		// so it may only be set on the content that was checked. The row was
		// read outside this transaction; the lengths read then guard the write,
		// and a row rewritten in between is left at 0 for the next purge. The
		// literal bodies_stripped = 0 also leaves a row another node's purge
		// got to first, and lets PostgreSQL find the row through the partial
		// index.
		guard := `bodies_stripped = 0 AND log_id = ?
			 AND length(input_content) = ? AND length(output_content) = ? AND length(detail_content) = ?`
		guardArgs := []any{row.LogID, row.InputLen, row.OutputLen, int64(len(row.Detail))}
		var written sql.Result
		if plan.leavesEmpty {
			written, err = tx.ExecContext(ctx, `DELETE FROM request_log_content WHERE `+guard+` AND length(session_id) = 0`, guardArgs...)
		} else {
			written, err = tx.ExecContext(ctx,
				`UPDATE request_log_content SET `+strings.Join(append(plan.sets, "bodies_stripped = 1"), ", ")+` WHERE `+guard,
				append(plan.args, guardArgs...)...,
			)
		}
		if err != nil {
			return fmt.Errorf("usage: strip request log content log_id=%d: %w", row.LogID, err)
		}
		if n, errRows := rowsAffected(written); errRows != nil {
			return fmt.Errorf("usage: strip request log content log_id=%d: %w", row.LogID, errRows)
		} else if n == 0 {
			continue
		}
		if plan.leavesEmpty {
			result.DeletedContents++
		}
		if plan.clearsBodies {
			result.ClearedBodyRows++
		}
		if plan.sanitizesDetail {
			result.SanitizedDetailRows++
			result.RemovedDetailBytes += plan.removedDetailBytes
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit request log body purge batch: %w", err)
	}
	committed = true
	return nil
}

func loadUnstrippedContentRows(ctx context.Context, db *sql.DB, ids []int64) ([]unstrippedContentRow, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, unstrippedContentRowsSQL(len(ids)), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: load unstripped request log content: %w", err)
	}
	defer rows.Close()
	batch := make([]unstrippedContentRow, 0, len(ids))
	for rows.Next() {
		var row unstrippedContentRow
		var failed int64
		if err = rows.Scan(&row.LogID, &row.Compression, &row.Detail, &row.InputLen, &row.OutputLen, &row.SessionLen, &failed); err != nil {
			return nil, fmt.Errorf("usage: scan unstripped request log content: %w", err)
		}
		row.Failed = failed == 1
		batch = append(batch, row)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterate unstripped request log content: %w", err)
	}
	return batch, nil
}

// contentStripPlan is what stripping one row changes. sets and args are the
// SET clauses beyond bodies_stripped and their values.
type contentStripPlan struct {
	sets               []string
	args               []any
	clearsBodies       bool
	sanitizesDetail    bool
	removedDetailBytes int64
	leavesEmpty        bool
}

// planContentStrip applies to a stored row what insertLogContentTenantTx does
// to a row written with body storage off: the request body goes, the response
// body goes unless the request failed (the error modal reads the compact
// upstream error there), and the detail keeps its metadata without the bodies.
// A row left with nothing in it is deleted, as clearRequestLogs does.
func planContentStrip(row unstrippedContentRow) (contentStripPlan, error) {
	var plan contentStripPlan
	if row.InputLen > 0 {
		plan.sets = append(plan.sets, "input_content = X''")
		plan.clearsBodies = true
	}
	keepsOutput := row.OutputLen > 0 && row.Failed
	if row.OutputLen > 0 && !keepsOutput {
		plan.sets = append(plan.sets, "output_content = X''")
		plan.clearsBodies = true
	}
	detailLeft := len(row.Detail) > 0
	if detailLeft {
		detail, err := decompressLogContent(row.Compression, row.Detail)
		if err != nil {
			return plan, fmt.Errorf("usage: decompress request detail log_id=%d: %w", row.LogID, err)
		}
		if sanitized, changed := sanitizeStoredRequestDetailBodies(detail); changed {
			compressed, errCompress := compressLogContent(sanitized)
			if errCompress != nil {
				return plan, fmt.Errorf("usage: compress request detail log_id=%d: %w", row.LogID, errCompress)
			}
			plan.sets = append(plan.sets, "detail_content = ?")
			plan.args = append(plan.args, compressed)
			plan.sanitizesDetail = true
			if delta := int64(len(row.Detail) - len(compressed)); delta > 0 {
				plan.removedDetailBytes = delta
			}
			detailLeft = len(compressed) > 0
		}
	}
	plan.leavesEmpty = !keepsOutput && !detailLeft && row.SessionLen == 0
	return plan, nil
}

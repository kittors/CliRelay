package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
)

const requestDetailSanitizeBatchSize = 25

var requestLogBodyPurgeMu sync.Mutex

type RequestLogBodyPurgeResult struct {
	ClearRequestLogsResult
	SanitizedDetailRows     int64 `json:"sanitized_detail_rows"`
	RemovedDetailBytes      int64 `json:"removed_detail_bytes"`
	ReclaimedStorage        bool  `json:"reclaimed_storage"`
	PhysicalReclaimDeferred bool  `json:"physical_reclaim_deferred"`
}

type storedRequestDetailRow struct {
	LogID       int64
	Compression string
	Content     []byte
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

// purgeStoredRequestBodies walks every stored request detail, so it can run for
// many minutes on a large table. ctx stops it between statements and cancels the
// one in flight: the maintenance worker passes its own context, and CloseDB waits
// for that worker before the process can exit. Each batch commits on its own, so
// a stopped purge loses nothing and the next pass walks the table again.
func purgeStoredRequestBodies(ctx context.Context, db *sql.DB, _ string) (RequestLogBodyPurgeResult, error) {
	requestLogBodyPurgeMu.Lock()
	defer requestLogBodyPurgeMu.Unlock()

	if db == nil {
		return RequestLogBodyPurgeResult{}, fmt.Errorf("usage: database not initialised")
	}

	cleared, err := clearRequestLogs(ctx, db, ClearRequestLogsOptions{ClearBodyContent: true})
	if err != nil {
		return RequestLogBodyPurgeResult{}, err
	}
	result := RequestLogBodyPurgeResult{ClearRequestLogsResult: cleared}

	result.SanitizedDetailRows, result.RemovedDetailBytes, err = sanitizeHistoricalRequestDetails(ctx, db)
	if err != nil {
		return result, err
	}
	refreshRequestLogContentBytes(db)

	changed := result.ClearedBodyRows > 0 || result.ClearedLegacyRows > 0 || result.SanitizedDetailRows > 0
	result.PhysicalReclaimDeferred = changed

	log.Infof(
		"usage: purged request log bodies (body_rows=%d legacy_rows=%d sanitized_detail_rows=%d removed_detail_bytes=%d physical_reclaim_deferred=%t)",
		result.ClearedBodyRows,
		result.ClearedLegacyRows,
		result.SanitizedDetailRows,
		result.RemovedDetailBytes,
		result.PhysicalReclaimDeferred,
	)
	return result, nil
}

func sanitizeHistoricalRequestDetails(ctx context.Context, db *sql.DB) (int64, int64, error) {
	var sanitizedRows int64
	var removedBytes int64
	var lastLogID int64

	for {
		if err := ctx.Err(); err != nil {
			return sanitizedRows, removedBytes, err
		}
		rows, err := db.QueryContext(ctx,
			`SELECT log_id, compression, detail_content
			 FROM request_log_content
			 WHERE log_id > ? AND length(detail_content) > 0
			 ORDER BY log_id ASC
			 LIMIT ?`,
			lastLogID,
			requestDetailSanitizeBatchSize,
		)
		if err != nil {
			return sanitizedRows, removedBytes, fmt.Errorf("usage: query historical request details: %w", err)
		}

		batch := make([]storedRequestDetailRow, 0, requestDetailSanitizeBatchSize)
		for rows.Next() {
			var row storedRequestDetailRow
			if err = rows.Scan(&row.LogID, &row.Compression, &row.Content); err != nil {
				_ = rows.Close()
				return sanitizedRows, removedBytes, fmt.Errorf("usage: scan historical request detail: %w", err)
			}
			batch = append(batch, row)
			lastLogID = row.LogID
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return sanitizedRows, removedBytes, fmt.Errorf("usage: iterate historical request details: %w", err)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			break
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return sanitizedRows, removedBytes, fmt.Errorf("usage: begin historical request detail cleanup: %w", err)
		}
		for _, row := range batch {
			detail, errDecode := decompressLogContent(row.Compression, row.Content)
			if errDecode != nil {
				_ = tx.Rollback()
				return sanitizedRows, removedBytes, fmt.Errorf("usage: decompress request detail log_id=%d: %w", row.LogID, errDecode)
			}
			sanitized, changed := sanitizeStoredRequestDetailBodies(detail)
			if !changed {
				continue
			}
			compressed, errCompress := compressLogContent(sanitized)
			if errCompress != nil {
				_ = tx.Rollback()
				return sanitizedRows, removedBytes, fmt.Errorf("usage: compress request detail log_id=%d: %w", row.LogID, errCompress)
			}
			if _, err = tx.ExecContext(ctx,
				"UPDATE request_log_content SET detail_content = ? WHERE log_id = ?",
				compressed,
				row.LogID,
			); err != nil {
				_ = tx.Rollback()
				return sanitizedRows, removedBytes, fmt.Errorf("usage: update request detail log_id=%d: %w", row.LogID, err)
			}
			sanitizedRows++
			if delta := int64(len(row.Content) - len(compressed)); delta > 0 {
				removedBytes += delta
			}
		}
		if err = tx.Commit(); err != nil {
			return sanitizedRows, removedBytes, fmt.Errorf("usage: commit historical request detail cleanup: %w", err)
		}
	}
	return sanitizedRows, removedBytes, nil
}

package usage

import (
	"database/sql"
	"fmt"
	"sync"

	log "github.com/sirupsen/logrus"
)

const requestDetailSanitizeBatchSize = 25

var requestLogBodyPurgeMu sync.Mutex

type RequestLogBodyPurgeResult struct {
	ClearRequestLogsResult
	SanitizedDetailRows int64 `json:"sanitized_detail_rows"`
	RemovedDetailBytes  int64 `json:"removed_detail_bytes"`
	ReclaimedStorage    bool  `json:"reclaimed_storage"`
}

type storedRequestDetailRow struct {
	LogID       int64
	Compression string
	Content     []byte
}

// PurgeStoredRequestBodies removes direct request/response bodies, strips bodies
// embedded in historical request details, and rewrites the physical content
// table when rows changed so PostgreSQL returns the freed space to the OS.
func PurgeStoredRequestBodies() (RequestLogBodyPurgeResult, error) {
	usageDBMu.Lock()
	db := usageDB
	driver := usageDriver
	usageDBMu.Unlock()
	if db == nil {
		return RequestLogBodyPurgeResult{}, fmt.Errorf("usage: database not initialised")
	}
	return purgeStoredRequestBodies(db, driver)
}

func purgeStoredRequestBodies(db *sql.DB, driver string) (RequestLogBodyPurgeResult, error) {
	requestLogBodyPurgeMu.Lock()
	defer requestLogBodyPurgeMu.Unlock()

	if db == nil {
		return RequestLogBodyPurgeResult{}, fmt.Errorf("usage: database not initialised")
	}

	cleared, err := clearRequestLogs(db, ClearRequestLogsOptions{ClearBodyContent: true})
	if err != nil {
		return RequestLogBodyPurgeResult{}, err
	}
	result := RequestLogBodyPurgeResult{ClearRequestLogsResult: cleared}

	result.SanitizedDetailRows, result.RemovedDetailBytes, err = sanitizeHistoricalRequestDetails(db)
	if err != nil {
		return result, err
	}
	refreshRequestLogContentBytes(db)

	changed := result.ClearedBodyRows > 0 || result.ClearedLegacyRows > 0 || result.SanitizedDetailRows > 0
	if changed {
		if err = reclaimRequestLogContentStorage(db, driver); err != nil {
			return result, err
		}
		result.ReclaimedStorage = true
	}

	log.Infof(
		"usage: purged request log bodies (body_rows=%d legacy_rows=%d sanitized_detail_rows=%d removed_detail_bytes=%d reclaimed=%t)",
		result.ClearedBodyRows,
		result.ClearedLegacyRows,
		result.SanitizedDetailRows,
		result.RemovedDetailBytes,
		result.ReclaimedStorage,
	)
	return result, nil
}

func sanitizeHistoricalRequestDetails(db *sql.DB) (int64, int64, error) {
	var sanitizedRows int64
	var removedBytes int64
	var lastLogID int64

	for {
		rows, err := db.Query(
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

		tx, err := db.Begin()
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
			if _, err = tx.Exec(
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

func reclaimRequestLogContentStorage(db *sql.DB, driver string) error {
	switch driver {
	case "postgres":
		if _, err := db.Exec("VACUUM (FULL, ANALYZE) request_log_content"); err != nil {
			return fmt.Errorf("usage: reclaim postgres request log content storage: %w", err)
		}
	case "sqlite":
		if _, err := db.Exec("VACUUM"); err != nil {
			return fmt.Errorf("usage: reclaim sqlite request log content storage: %w", err)
		}
	}
	return nil
}

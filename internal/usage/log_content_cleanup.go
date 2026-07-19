package usage

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
)

// Request log content cleanup contract:
// - Owner: usage/request log persistence boundary.
// - Responsibility: trimming oversized stored content, retention cleanup, and reclaim-oriented content pruning.
// - Non-goals: request log file retention and forced error-log cleanup in internal/logging.
type logContentQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func cleanupOversizedLogContent(db *sql.DB, maxBytes int64) (int64, error) {
	if db == nil {
		return 0, nil
	}
	return cleanupOversizedLogContentQuerier(db, maxBytes)
}

func cleanupOversizedLogContentQuerier(q logContentQuerier, maxBytes int64) (int64, error) {
	if q == nil || maxBytes <= 0 {
		return 0, nil
	}

	totalBytes, err := queryStoredContentBytes(q)
	if err != nil {
		return 0, err
	}

	_, deletedRows, err := cleanupOversizedLogContentQuerierWithTotalInternal(q, totalBytes, maxBytes)
	return deletedRows, err
}

func cleanupOversizedLogContentQuerierWithTotal(q logContentQuerier, totalBytes int64, maxBytes int64) (int64, error) {
	if q == nil || maxBytes <= 0 || totalBytes <= maxBytes {
		return 0, nil
	}
	trimmedBytes, _, err := cleanupOversizedLogContentQuerierWithTotalInternal(q, totalBytes, maxBytes)
	return trimmedBytes, err
}

func cleanupOversizedLogContentQuerierWithTotalInternal(q logContentQuerier, totalBytes int64, maxBytes int64) (int64, int64, error) {
	if q == nil || maxBytes <= 0 || totalBytes <= maxBytes {
		return 0, 0, nil
	}

	var deletedRows int64
	var deletedBytes int64
	for totalBytes > maxBytes {
		required := totalBytes - maxBytes
		ids, reclaimed, err := oldestContentRowsForTrim(q, required, 200)
		if err != nil {
			return deletedBytes, deletedRows, err
		}
		if len(ids) == 0 || reclaimed <= 0 {
			break
		}
		query, args := buildDeleteContentRowsQuery(ids)
		result, err := q.Exec(query, args...)
		if err != nil {
			return deletedBytes, deletedRows, fmt.Errorf("usage: delete oversized content rows: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return deletedBytes, deletedRows, fmt.Errorf("usage: affected rows for oversized content cleanup: %w", err)
		}
		deletedRows += affected
		deletedBytes += reclaimed
		totalBytes -= reclaimed
	}
	return deletedBytes, deletedRows, nil
}

func queryStoredContentBytes(q logContentQuerier) (int64, error) {
	var totalBytes sql.NullInt64
	err := q.QueryRow(
		`SELECT COALESCE(SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) + CAST(length(detail_content) AS INTEGER)), 0)
		 FROM request_log_content`,
	).Scan(&totalBytes)
	if err != nil {
		return 0, fmt.Errorf("usage: query stored content bytes: %w", err)
	}
	if !totalBytes.Valid {
		return 0, nil
	}
	return totalBytes.Int64, nil
}

func oldestContentRowsForTrim(q logContentQuerier, requiredBytes int64, limit int) ([]int64, int64, error) {
	if q == nil || requiredBytes <= 0 {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 200
	}

	rows, err := q.Query(
		`SELECT log_id, CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) + CAST(length(detail_content) AS INTEGER) AS size
		 FROM request_log_content
		 ORDER BY timestamp ASC, log_id ASC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("usage: query oldest content rows: %w", err)
	}
	defer rows.Close()

	ids := make([]int64, 0, limit)
	var reclaimed int64
	for rows.Next() {
		var (
			logID int64
			size  int64
		)
		if err := rows.Scan(&logID, &size); err != nil {
			return nil, 0, fmt.Errorf("usage: scan oldest content row: %w", err)
		}
		ids = append(ids, logID)
		reclaimed += size
		if reclaimed >= requiredBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("usage: iterate oldest content rows: %w", err)
	}
	return ids, reclaimed, nil
}

func buildDeleteContentRowsQuery(ids []int64) (string, []any) {
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	query := fmt.Sprintf("DELETE FROM request_log_content WHERE log_id IN (%s)", string(placeholders))
	return query, args
}

func compactLogContentStorage(db *sql.DB) {
	if db == nil {
		return
	}
	compactLogContentStorageInternal(db, true)
}

type sqliteSpaceStats struct {
	PageSize      int64
	PageCount     int64
	FreeListCount int64
}

func querySQLiteSpaceStats(q logContentQuerier) (sqliteSpaceStats, error) {
	if q == nil {
		return sqliteSpaceStats{}, fmt.Errorf("usage: nil querier")
	}
	var pageSize int64
	if err := q.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return sqliteSpaceStats{}, err
	}
	var pageCount int64
	if err := q.QueryRow("PRAGMA page_count").Scan(&pageCount); err != nil {
		return sqliteSpaceStats{}, err
	}
	var freeListCount int64
	if err := q.QueryRow("PRAGMA freelist_count").Scan(&freeListCount); err != nil {
		return sqliteSpaceStats{}, err
	}
	return sqliteSpaceStats{
		PageSize:      pageSize,
		PageCount:     pageCount,
		FreeListCount: freeListCount,
	}, nil
}

func reclaimableBytes(stats sqliteSpaceStats) int64 {
	if stats.PageSize <= 0 || stats.FreeListCount <= 0 {
		return 0
	}
	return stats.PageSize * stats.FreeListCount
}

func shouldVacuum(stats sqliteSpaceStats) bool {
	if stats.PageSize <= 0 || stats.PageCount <= 0 || stats.FreeListCount <= 0 {
		return false
	}

	freeBytes := reclaimableBytes(stats)
	if freeBytes < sqliteVacuumMinReclaimBytes {
		ratio := float64(stats.FreeListCount) / float64(stats.PageCount)
		return ratio >= sqliteVacuumMinReclaimRatio && freeBytes >= (sqliteVacuumMinReclaimBytes/2)
	}
	return true
}

func vacuumAllowedNow(now time.Time) bool {
	lastNano := lastUsageVacuumUnixNano.Load()
	if lastNano <= 0 {
		return true
	}
	last := time.Unix(0, lastNano)
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= sqliteVacuumMinInterval
}

func markVacuumRan(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	lastUsageVacuumUnixNano.Store(now.UnixNano())
}

func usageWALPath() string {
	if usageDBPath == "" {
		return ""
	}
	return usageDBPath + "-wal"
}

func walBytesOnDisk() int64 {
	path := usageWALPath()
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func compactLogContentStorageInternal(db *sql.DB, allowOptimize bool) {
	if db == nil {
		return
	}
	if usageDriver == "postgres" {
		if err := compactPostgresLogStorage(db); err != nil {
			log.Warnf("usage: postgres log storage compact skipped: %v", err)
		}
		return
	}

	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Warnf("usage: wal checkpoint failed: %v", err)
	}

	stats, errStats := querySQLiteSpaceStats(db)
	if errStats != nil {
		if allowOptimize {
			if _, err := db.Exec("PRAGMA optimize"); err != nil {
				log.Warnf("usage: sqlite optimize failed: %v", err)
			}
		}
		return
	}

	didVacuum := false
	now := time.Now()
	if currentRequestLogStorageConfig().VacuumOnCleanup && shouldVacuum(stats) && vacuumAllowedNow(now) {
		freeBytes := reclaimableBytes(stats)
		log.Infof("usage: reclaimable sqlite free space detected (freelist=%d pages, approx=%d bytes), running VACUUM", stats.FreeListCount, freeBytes)
		if _, err := db.Exec("VACUUM"); err != nil {
			log.Warnf("usage: vacuum failed: %v", err)
		} else {
			didVacuum = true
			markVacuumRan(now)
		}
	}

	if allowOptimize || didVacuum {
		if _, err := db.Exec("PRAGMA optimize"); err != nil {
			log.Warnf("usage: sqlite optimize failed: %v", err)
		}
	}

	if walBytes := walBytesOnDisk(); walBytes > 0 && walBytes >= (64<<20) {
		log.Warnf("usage: sqlite WAL remains large after checkpoint (%d bytes at %s); consider lowering cleanup-interval-minutes or checking long-lived transactions", walBytes, usageWALPath())
	}
}

func compactPostgresLogStorage(db *sql.DB) error {
	if db == nil {
		return nil
	}
	return nil
}

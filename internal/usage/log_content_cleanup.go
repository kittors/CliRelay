package usage

import (
	"context"
	"database/sql"
	"fmt"

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
		ids, reclaimed, err := oldestContentRowsForTrim(q, required, 200, true)
		if err != nil {
			return deletedBytes, deletedRows, err
		}
		if len(ids) == 0 || reclaimed <= 0 {
			// Only failure diagnostics are left. The cap still has to hold, so
			// fall back to plain FIFO rather than let the table grow unbounded.
			ids, reclaimed, err = oldestContentRowsForTrim(q, required, 200, false)
			if err != nil {
				return deletedBytes, deletedRows, err
			}
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

// oldestContentRowsForTrim picks the rows to drop when stored content exceeds
// the size cap.
//
// preserveFailed keeps failure diagnostics out of that first pass. A failed
// request stores a few hundred bytes of upstream error; a successful one stores
// a request and response body that routinely run into hundreds of kilobytes.
// Under plain FIFO the successes consume the entire budget and carry the
// diagnostics out with them, so a 3-day retention setting held under an hour of
// rows in production and the upstream error explaining an incident was gone
// before anyone opened the log. Evicting successes first costs little — they
// are what fills the cap — and buys the error detail the retention it was
// configured for.
//
// The lookup stays on the timestamp index and probes request_logs by primary
// key per candidate; failures are a small minority, so the scan reaches its
// limit in about as many rows as before. It matches on log_id alone, as the
// delete below does: log ids come from one sequence, and leaving tenant_id out
// can only ever keep a diagnostic row longer, never drop one early.
func oldestContentRowsForTrim(q logContentQuerier, requiredBytes int64, limit int, preserveFailed bool) ([]int64, int64, error) {
	if q == nil || requiredBytes <= 0 {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 200
	}

	query := `SELECT log_id, CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) + CAST(length(detail_content) AS INTEGER) AS size
		 FROM request_log_content
		 ORDER BY timestamp ASC, log_id ASC
		 LIMIT ?`
	if preserveFailed {
		query = `SELECT c.log_id, CAST(length(c.input_content) AS INTEGER) + CAST(length(c.output_content) AS INTEGER) + CAST(length(c.detail_content) AS INTEGER) AS size
		 FROM request_log_content c
		 WHERE NOT EXISTS (SELECT 1 FROM request_logs l WHERE l.id = c.log_id AND l.failed = 1)
		 ORDER BY c.timestamp ASC, c.log_id ASC
		 LIMIT ?`
	}

	rows, err := q.Query(query, limit)
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
	compactLogContentStorageInternal(context.Background(), db)
}

func compactLogContentStorageInternal(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	if usageDriver != "postgres" {
		return
	}
	if err := compactPostgresLogStorage(ctx, db); err != nil {
		log.Warnf("usage: postgres log storage compact skipped: %v", err)
	}
}

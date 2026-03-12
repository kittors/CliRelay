package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const requestLogContentCompression = "zstd"

type requestLogStorageRuntime struct {
	StoreContent           bool
	ContentRetentionDays   int
	CleanupIntervalMinutes int
	MaxTotalSizeMB         int
	VacuumOnCleanup        bool
}

var (
	requestLogStorage = requestLogStorageRuntime{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
		MaxTotalSizeMB:         0,
		VacuumOnCleanup:        true,
	}

	requestLogMaintenanceCancel context.CancelFunc
	requestLogMaintenanceWG     sync.WaitGroup

	zstdEncoderPool = sync.Pool{
		New: func() any {
			encoder, err := zstd.NewWriter(nil)
			if err != nil {
				panic(err)
			}
			return encoder
		},
	}
	zstdDecoderPool = sync.Pool{
		New: func() any {
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				panic(err)
			}
			return decoder
		},
	}
)

func contentRetentionUnlimited() bool {
	return requestLogStorage.ContentRetentionDays <= 0
}

func normalizeRequestLogStorageConfig(cfg config.RequestLogStorageConfig) requestLogStorageRuntime {
	if !cfg.StoreContent && cfg.ContentRetentionDays == 0 && cfg.CleanupIntervalMinutes == 0 && !cfg.VacuumOnCleanup {
		return requestLogStorageRuntime{
			StoreContent:           true,
			ContentRetentionDays:   30,
			CleanupIntervalMinutes: 1440,
			MaxTotalSizeMB:         0,
			VacuumOnCleanup:        true,
		}
	}

	runtimeCfg := requestLogStorageRuntime{
		StoreContent:           cfg.StoreContent,
		ContentRetentionDays:   cfg.ContentRetentionDays,
		CleanupIntervalMinutes: cfg.CleanupIntervalMinutes,
		MaxTotalSizeMB:         cfg.MaxTotalSizeMB,
		VacuumOnCleanup:        cfg.VacuumOnCleanup,
	}
	if runtimeCfg.ContentRetentionDays < 0 {
		runtimeCfg.ContentRetentionDays = 0
	}
	if runtimeCfg.CleanupIntervalMinutes <= 0 {
		runtimeCfg.CleanupIntervalMinutes = 1440
	}
	if runtimeCfg.MaxTotalSizeMB < 0 {
		runtimeCfg.MaxTotalSizeMB = 0
	}
	return runtimeCfg
}

func maxLogContentBytes() int64 {
	if requestLogStorage.MaxTotalSizeMB <= 0 {
		return 0
	}
	return int64(requestLogStorage.MaxTotalSizeMB) * 1024 * 1024
}

func startRequestLogMaintenance(db *sql.DB) {
	stopRequestLogMaintenance()
	if db == nil || !requestLogStorage.StoreContent {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	requestLogMaintenanceCancel = cancel
	requestLogMaintenanceWG.Add(1)
	go func() {
		defer requestLogMaintenanceWG.Done()
		runRequestLogMaintenancePass(db)

		ticker := time.NewTicker(time.Duration(requestLogStorage.CleanupIntervalMinutes) * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runRequestLogMaintenancePass(db)
			}
		}
	}()
}

func stopRequestLogMaintenance() {
	if requestLogMaintenanceCancel != nil {
		requestLogMaintenanceCancel()
		requestLogMaintenanceWG.Wait()
		requestLogMaintenanceCancel = nil
	}
}

func runRequestLogMaintenancePass(db *sql.DB) {
	if db == nil {
		return
	}

	for {
		migrated, err := migrateLegacyContentBatch(db, 200)
		if err != nil {
			log.Errorf("usage: migrate legacy request log content: %v", err)
			break
		}
		if migrated == 0 {
			break
		}
	}

	deleted, err := cleanupExpiredLogContent(db)
	if err != nil {
		log.Errorf("usage: cleanup request log content: %v", err)
		return
	}
	if deleted > 0 {
		log.Infof("usage: pruned %d expired request log content rows", deleted)
	}

	trimmed, err := cleanupOversizedLogContent(db, maxLogContentBytes())
	if err != nil {
		log.Errorf("usage: enforce request log content size cap: %v", err)
		return
	}
	if trimmed > 0 {
		log.Infof("usage: pruned %d request log content rows to enforce size cap", trimmed)
	}

	if deleted+trimmed > 0 {
		compactLogContentStorage(db)
	}
}

func insertLogContentTx(tx *sql.Tx, logID int64, timestamp time.Time, inputContent, outputContent string) error {
	if tx == nil || logID < 1 || (!requestLogStorage.StoreContent) {
		return nil
	}

	inputCompressed, err := compressLogContent(inputContent)
	if err != nil {
		return err
	}
	outputCompressed, err := compressLogContent(outputContent)
	if err != nil {
		return err
	}

	rowBytes := int64(len(inputCompressed) + len(outputCompressed))
	maxBytes := maxLogContentBytes()
	if maxBytes > 0 && rowBytes > maxBytes {
		log.Warnf("usage: skip storing request log content for log_id=%d because compressed body %d bytes exceeds configured cap %d bytes", logID, rowBytes, maxBytes)
		return nil
	}

	_, err = tx.Exec(
		`INSERT INTO request_log_content (log_id, timestamp, compression, input_content, output_content)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(log_id) DO UPDATE SET
		   timestamp = excluded.timestamp,
		   compression = excluded.compression,
		   input_content = excluded.input_content,
		   output_content = excluded.output_content`,
		logID,
		timestamp.UTC().Format(time.RFC3339Nano),
		requestLogContentCompression,
		inputCompressed,
		outputCompressed,
	)
	if err != nil {
		return fmt.Errorf("usage: insert compressed content: %w", err)
	}
	if maxBytes > 0 {
		if _, err := cleanupOversizedLogContentQuerier(tx, maxBytes); err != nil {
			return fmt.Errorf("usage: enforce content size cap: %w", err)
		}
	}
	return nil
}

func compressLogContent(content string) ([]byte, error) {
	if content == "" {
		return []byte{}, nil
	}
	encoder := zstdEncoderPool.Get().(*zstd.Encoder)
	defer zstdEncoderPool.Put(encoder)
	return encoder.EncodeAll([]byte(content), make([]byte, 0, len(content)/2)), nil
}

func decompressLogContent(compression string, content []byte) (string, error) {
	if len(content) == 0 {
		return "", nil
	}
	switch compression {
	case "", requestLogContentCompression:
		decoder := zstdDecoderPool.Get().(*zstd.Decoder)
		defer zstdDecoderPool.Put(decoder)
		decoded, err := decoder.DecodeAll(content, nil)
		if err != nil {
			return "", fmt.Errorf("usage: decompress content: %w", err)
		}
		return string(decoded), nil
	default:
		return "", fmt.Errorf("usage: unsupported content compression %q", compression)
	}
}

func migrateLegacyContentBatch(db *sql.DB, batchSize int) (int, error) {
	if db == nil || !requestLogStorage.StoreContent {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	rows, err := db.Query(
		`SELECT id, timestamp, input_content, output_content
		 FROM request_logs
		 WHERE (length(input_content) > 0 OR length(output_content) > 0)
		   AND NOT EXISTS (SELECT 1 FROM request_log_content content WHERE content.log_id = request_logs.id)
		 ORDER BY id
		 LIMIT ?`,
		batchSize,
	)
	if err != nil {
		return 0, fmt.Errorf("usage: query legacy content rows: %w", err)
	}
	defer rows.Close()

	type legacyRow struct {
		ID            int64
		Timestamp     string
		InputContent  string
		OutputContent string
	}

	batch := make([]legacyRow, 0, batchSize)
	for rows.Next() {
		var row legacyRow
		if err := rows.Scan(&row.ID, &row.Timestamp, &row.InputContent, &row.OutputContent); err != nil {
			return 0, fmt.Errorf("usage: scan legacy content row: %w", err)
		}
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("usage: iterate legacy content rows: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, fmt.Errorf("usage: begin legacy migration tx: %w", err)
	}

	for _, row := range batch {
		timestamp, errParse := time.Parse(time.RFC3339Nano, row.Timestamp)
		if errParse != nil {
			timestamp = time.Now().UTC()
		}

		shouldKeep := requestLogStorage.StoreContent && withinContentRetention(timestamp)
		if shouldKeep {
			if errStore := insertLogContentTx(tx, row.ID, timestamp, row.InputContent, row.OutputContent); errStore != nil {
				_ = tx.Rollback()
				return 0, errStore
			}
		}

		if _, errUpdate := tx.Exec(
			"UPDATE request_logs SET input_content = '', output_content = '' WHERE id = ?",
			row.ID,
		); errUpdate != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("usage: clear legacy content columns: %w", errUpdate)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("usage: commit legacy migration: %w", err)
	}
	return len(batch), nil
}

func withinContentRetention(timestamp time.Time) bool {
	if contentRetentionUnlimited() {
		return true
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -requestLogStorage.ContentRetentionDays)
	return !timestamp.Before(cutoff)
}

func cleanupExpiredLogContent(db *sql.DB) (int64, error) {
	if db == nil || !requestLogStorage.StoreContent || contentRetentionUnlimited() {
		return 0, nil
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -requestLogStorage.ContentRetentionDays).Format(time.RFC3339Nano)
	result, err := db.Exec("DELETE FROM request_log_content WHERE timestamp < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("usage: delete expired content: %w", err)
	}

	legacyResult, err := db.Exec(
		"UPDATE request_logs SET input_content = '', output_content = '' WHERE timestamp < ? AND (length(input_content) > 0 OR length(output_content) > 0)",
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("usage: clear expired legacy content: %w", err)
	}
	legacyCleared, err := legacyResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows for legacy content cleanup: %w", err)
	}

	deletedRows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows for content cleanup: %w", err)
	}
	totalChanged := deletedRows + legacyCleared
	if totalChanged == 0 {
		return 0, nil
	}
	return totalChanged, nil
}

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

	var deletedRows int64
	for totalBytes > maxBytes {
		required := totalBytes - maxBytes
		ids, reclaimed, err := oldestContentRowsForTrim(q, required, 200)
		if err != nil {
			return deletedRows, err
		}
		if len(ids) == 0 || reclaimed <= 0 {
			break
		}
		query, args := buildDeleteContentRowsQuery(ids)
		result, err := q.Exec(query, args...)
		if err != nil {
			return deletedRows, fmt.Errorf("usage: delete oversized content rows: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return deletedRows, fmt.Errorf("usage: affected rows for oversized content cleanup: %w", err)
		}
		deletedRows += affected
		totalBytes -= reclaimed
	}
	return deletedRows, nil
}

func queryStoredContentBytes(q logContentQuerier) (int64, error) {
	var totalBytes sql.NullInt64
	err := q.QueryRow(
		`SELECT COALESCE(SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER)), 0)
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
		`SELECT log_id, CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) AS size
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
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Warnf("usage: wal checkpoint after content cleanup failed: %v", err)
	}
	if requestLogStorage.VacuumOnCleanup {
		if _, err := db.Exec("VACUUM"); err != nil {
			log.Warnf("usage: vacuum after content cleanup failed: %v", err)
		}
	}
	if _, err := db.Exec("PRAGMA optimize"); err != nil {
		log.Warnf("usage: sqlite optimize after content cleanup failed: %v", err)
	}
}

func queryCompressedLogContent(db *sql.DB, query string, args ...any) (LogContentResult, error) {
	if db == nil {
		return LogContentResult{}, fmt.Errorf("usage: database not initialised")
	}

	var (
		result           LogContentResult
		compression      string
		inputCompressed  []byte
		outputCompressed []byte
	)
	err := db.QueryRow(query, args...).Scan(
		&result.ID,
		&result.Model,
		&compression,
		&inputCompressed,
		&outputCompressed,
	)
	if err != nil {
		return LogContentResult{}, err
	}

	inputContent, err := decompressLogContent(compression, inputCompressed)
	if err != nil {
		return LogContentResult{}, err
	}
	outputContent, err := decompressLogContent(compression, outputCompressed)
	if err != nil {
		return LogContentResult{}, err
	}
	result.InputContent = inputContent
	result.OutputContent = outputContent
	return result, nil
}

package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// Request log content storage contract:
// - Owner: usage/request log persistence boundary.
// - Responsibility: compressed request/response/detail content writes, runtime storage policy, and maintenance scheduling.
// - Non-goals: human-readable file log formatting and transport-level request log orchestration.
const requestLogContentCompression = "zstd"

type requestLogStorageRuntime struct {
	RetentionDays            int
	ContentRetentionDays     int
	CleanupEnabled           bool
	CleanupIntervalMinutes   int
	CleanupBatchSize         int
	CleanupMaxRuntimeSeconds int
	MaxRows                  int
	MaxMetadataSizeMB        int
	MaxTotalSizeMB           int
}

var (
	requestLogStorage atomic.Value // requestLogStorageRuntime

	requestLogMaintenanceCancel context.CancelFunc
	requestLogMaintenanceWG     sync.WaitGroup
	requestLogMaintenanceWakeup atomic.Value // chan struct{}

	requestLogContentBytes  atomic.Int64 // total compressed bytes; -1 means unknown
	requestLogBodiesEnabled atomic.Bool

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

func init() {
	requestLogStorage.Store(defaultRequestLogStorageRuntime())
	requestLogContentBytes.Store(-1)
	requestLogBodiesEnabled.Store(false)
	// Initialize atomic.Value type so subsequent stores can use typed nil safely.
	requestLogMaintenanceWakeup.Store((chan struct{})(nil))
}

func defaultRequestLogStorageRuntime() requestLogStorageRuntime {
	return requestLogStorageRuntime{
		RetentionDays:            7,
		ContentRetentionDays:     3,
		CleanupEnabled:           true,
		CleanupIntervalMinutes:   60,
		CleanupBatchSize:         1000,
		CleanupMaxRuntimeSeconds: 30,
		MaxRows:                  100000,
		MaxMetadataSizeMB:        256,
		MaxTotalSizeMB:           128,
	}
}

func currentRequestLogStorageConfig() requestLogStorageRuntime {
	value := requestLogStorage.Load()
	if value == nil {
		return defaultRequestLogStorageRuntime()
	}
	runtimeCfg, _ := value.(requestLogStorageRuntime)
	return runtimeCfg
}

func contentRetentionUnlimited() bool {
	return currentRequestLogStorageConfig().ContentRetentionDays <= 0
}

// RequestLogBodyStorageEnabled reports whether full request and response bodies
// are currently persisted. Request details are stored independently.
func RequestLogBodyStorageEnabled() bool {
	return requestLogBodiesEnabled.Load()
}

// SetRequestLogBodyStorageEnabled applies the body-storage toggle immediately
// without requiring a process restart.
func SetRequestLogBodyStorageEnabled(enabled bool) {
	requestLogBodiesEnabled.Store(enabled)
}

// ApplyRequestLogStorageConfig applies the complete request-log body policy at
// runtime. The maintenance loop reads the atomic snapshot on each pass, so YAML
// changes do not require a process restart.
func ApplyRequestLogStorageConfig(cfg config.RequestLogStorageConfig) {
	requestLogStorage.Store(normalizeRequestLogStorageConfig(cfg))
	SetRequestLogBodyStorageEnabled(cfg.StoreContent)
	triggerRequestLogCompaction()
}

func normalizeRequestLogStorageConfig(cfg config.RequestLogStorageConfig) requestLogStorageRuntime {
	runtimeCfg := requestLogStorageRuntime{
		RetentionDays:            cfg.RetentionDays,
		ContentRetentionDays:     cfg.ContentRetentionDays,
		CleanupEnabled:           true,
		CleanupIntervalMinutes:   cfg.CleanupIntervalMinutes,
		CleanupBatchSize:         cfg.CleanupBatchSize,
		CleanupMaxRuntimeSeconds: cfg.CleanupMaxRuntimeSeconds,
		MaxRows:                  cfg.MaxRows,
		MaxMetadataSizeMB:        cfg.MaxMetadataSizeMB,
		MaxTotalSizeMB:           cfg.MaxTotalSizeMB,
	}
	if cfg.CleanupEnabled != nil {
		runtimeCfg.CleanupEnabled = *cfg.CleanupEnabled
	}
	if runtimeCfg.RetentionDays <= 0 {
		runtimeCfg.RetentionDays = 7
	}
	if runtimeCfg.ContentRetentionDays < 0 {
		runtimeCfg.ContentRetentionDays = 0
	}
	if runtimeCfg.ContentRetentionDays > runtimeCfg.RetentionDays {
		runtimeCfg.ContentRetentionDays = runtimeCfg.RetentionDays
	}
	if runtimeCfg.CleanupIntervalMinutes <= 0 {
		runtimeCfg.CleanupIntervalMinutes = 60
	}
	if runtimeCfg.CleanupBatchSize <= 0 {
		runtimeCfg.CleanupBatchSize = 1000
	}
	if runtimeCfg.CleanupMaxRuntimeSeconds <= 0 {
		runtimeCfg.CleanupMaxRuntimeSeconds = 30
	}
	if runtimeCfg.MaxRows < 0 {
		runtimeCfg.MaxRows = 0
	}
	if runtimeCfg.MaxMetadataSizeMB < 0 {
		runtimeCfg.MaxMetadataSizeMB = 0
	}
	if runtimeCfg.MaxTotalSizeMB < 0 {
		runtimeCfg.MaxTotalSizeMB = 0
	}
	return runtimeCfg
}

func maxLogContentBytes() int64 {
	runtimeCfg := currentRequestLogStorageConfig()
	if runtimeCfg.MaxTotalSizeMB <= 0 {
		return 0
	}
	return int64(runtimeCfg.MaxTotalSizeMB) * 1024 * 1024
}

func requestLogMaintenanceWakeupChan() chan struct{} {
	value := requestLogMaintenanceWakeup.Load()
	if value == nil {
		return nil
	}
	ch, _ := value.(chan struct{})
	return ch
}

func triggerRequestLogCompaction() {
	ch := requestLogMaintenanceWakeupChan()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func startRequestLogMaintenance(db *sql.DB, driver string) {
	stopRequestLogMaintenance()
	if db == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	requestLogMaintenanceCancel = cancel
	wakeup := make(chan struct{}, 1)
	requestLogMaintenanceWakeup.Store(wakeup)
	postgresRequestLogHygieneDone.Store(false)
	requestLogMaintenanceWG.Add(1)
	// 请求日志维护协程属于 usage 存储子系统：
	// - owner: startRequestLogMaintenance / stopRequestLogMaintenance
	// - 取消条件: stopRequestLogMaintenance、数据库关闭、进程退出
	// - 超时策略: 周期 cleanup + wakeup 驱动；单次 DB 操作各自控制
	// - 清理方式: cancel 后等待 requestLogMaintenanceWG，确保协程退出
	go func() {
		defer requestLogMaintenanceWG.Done()
		runRequestLogMaintenancePass(ctx, db, driver)

		for {
			interval := time.Duration(currentRequestLogStorageConfig().CleanupIntervalMinutes) * time.Minute
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-wakeup:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				// Compaction/config wakeups should remain lightweight. The next
				// timer is rebuilt from the latest runtime policy.
				compactLogContentStorageInternal(ctx, db)
			case <-timer.C:
				runRequestLogMaintenancePass(ctx, db, driver)
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
	requestLogMaintenanceWakeup.Store((chan struct{})(nil))
}

func runRequestLogMaintenancePass(ctx context.Context, db *sql.DB, driver string) {
	if removed, err := CleanupStaleDeferredUsageContentFiles(); err != nil {
		log.WithError(err).Warn("usage: stale deferred content cleanup completed with errors")
	} else if removed > 0 {
		log.Infof("usage: removed %d stale deferred content file(s)", removed)
	}
	if db == nil {
		return
	}
	if !RequestLogBodyStorageEnabled() {
		if _, err := purgeStoredRequestBodies(db, driver); err != nil {
			log.Errorf("usage: purge disabled request log body storage: %v", err)
			return
		}
	}

	// Refresh the running total periodically so size-cap enforcement stays fast
	// and accurate without per-request full table scans.
	if requestLogContentBytes.Load() < 0 {
		if total, err := queryStoredContentBytes(db); err == nil {
			requestLogContentBytes.Store(total)
		}
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

	// Finish pending→done catch-up if startup goroutine has not yet (or failed).
	maybeFinalizeUsageRollupCatchup(db)

	// Rollup minute/hour/day retention is independent of detail cleanup-enabled.
	// Projection must stay bounded even when operators pause request_logs cleanup.
	if n, err := cleanupExpiredUsageRollupBuckets(db); err != nil {
		log.Errorf("usage: prune usage rollup buckets: %v", err)
	} else if n > 0 {
		log.Infof("usage: pruned %d expired usage_rollup_buckets rows", n)
	}
	if n, err := cleanupExpiredAIAccountSubjectUsageBuckets(db); err != nil {
		log.Errorf("usage: prune shared AI account day buckets: %v", err)
	} else if n > 0 {
		log.Infof("usage: pruned %d expired shared AI account day rows", n)
	}
	if n, err := cleanupExpiredAIAccountSubjectQuotaPoints(db); err != nil {
		log.Errorf("usage: prune shared AI account quota points: %v", err)
	} else if n > 0 {
		log.Infof("usage: pruned %d expired shared AI account quota point rows", n)
	}

	// cleanup-enabled gates metadata and content retention/size cleanup only.
	// Body purge when store-content=false still runs above (privacy, not retention).
	if !currentRequestLogStorageConfig().CleanupEnabled {
		compactLogContentStorageInternal(ctx, db)
		return
	}

	cleanupStarted := time.Now()
	metaDeleted, metaErr := cleanupExpiredRequestLogMetadata(ctx, db)
	if metaErr != nil {
		log.Errorf("usage: cleanup request log metadata: %v", metaErr)
		recordCleanupPass(metaDeleted, cleanupStarted, "error", metaErr.Error())
	} else if metaDeleted > 0 {
		log.Infof("usage: pruned %d expired request_logs metadata rows", metaDeleted)
		recordCleanupPass(metaDeleted, cleanupStarted, "ok", "")
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

	// After maintenance changes, refresh the exact total once to keep the running
	// counter accurate (avoids drift from pruning/migration deletes).
	if total, err := queryStoredContentBytes(db); err == nil {
		requestLogContentBytes.Store(total)
	} else {
		requestLogContentBytes.Store(-1)
	}

	// PostgreSQL compaction stays online and asynchronous; never wait for VACUUM FULL.
	compactLogContentStorageInternal(ctx, db)
}

func refreshRequestLogContentBytes(q logContentQuerier) {
	if total, err := queryStoredContentBytes(q); err == nil {
		requestLogContentBytes.Store(total)
	} else {
		requestLogContentBytes.Store(-1)
	}
}

func insertLogContentTx(tx *sql.Tx, logID int64, timestamp time.Time, inputContent, outputContent, detailContent string) error {
	return insertLogContentTenantTx(tx, "00000000-0000-0000-0000-000000000001", logID, timestamp, inputContent, outputContent, detailContent, false)
}

func insertLogContentTenantTx(tx *sql.Tx, tenantID string, logID int64, timestamp time.Time, inputContent, outputContent, detailContent string, failed bool) error {
	if tx == nil || logID < 1 {
		return nil
	}
	if !RequestLogBodyStorageEnabled() {
		// Privacy: do not keep full request/response bodies when body storage is off.
		// Failed requests are an exception for output_content: the management UI
		// error modal needs the compact upstream error payload (status/message JSON).
		inputContent = ""
		if !failed {
			outputContent = ""
		} else {
			outputContent = compactFailedOutputContent(outputContent)
		}
		detailContent = stripStoredRequestDetailBodies(detailContent)
	}
	if inputContent == "" && outputContent == "" && detailContent == "" {
		return nil
	}
	sessionID := extractSessionIDFromDetails(detailContent)

	inputCompressed, err := compressLogContent(inputContent)
	if err != nil {
		return err
	}
	outputCompressed, err := compressLogContent(outputContent)
	if err != nil {
		return err
	}
	detailCompressed, err := compressLogContent(detailContent)
	if err != nil {
		return err
	}

	rowBytes := int64(len(inputCompressed) + len(outputCompressed) + len(detailCompressed))
	maxBytes := maxLogContentBytes()
	if maxBytes > 0 && rowBytes > maxBytes {
		log.Warnf("usage: skip storing request log content for log_id=%d because compressed body %d bytes exceeds configured cap %d bytes", logID, rowBytes, maxBytes)
		return nil
	}

	tenantID = normalizeTenantID(tenantID)
	conflictTarget := "(log_id)"
	if usageDriver == "postgres" {
		conflictTarget = "(tenant_id, log_id)"
	}
	query := `INSERT INTO request_log_content (tenant_id, log_id, timestamp, compression, input_content, output_content, detail_content, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT` + conflictTarget + ` DO UPDATE SET
		   tenant_id = excluded.tenant_id,
		   timestamp = excluded.timestamp,
		   compression = excluded.compression,
		   input_content = excluded.input_content,
		   output_content = excluded.output_content,
		   detail_content = excluded.detail_content,
		   session_id = excluded.session_id`
	args := []any{tenantID, logID, timestamp.UTC().Format(time.RFC3339Nano), requestLogContentCompression, inputCompressed, outputCompressed, detailCompressed, sessionID}
	_, err = tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("usage: insert compressed content: %w", err)
	}
	if maxBytes > 0 {
		total := requestLogContentBytes.Load()
		if total >= 0 {
			// Fast path: keep a running total without scanning the whole table.
			total = requestLogContentBytes.Add(rowBytes)
		} else {
			// Bootstrap the running total once (may scan), then keep it updated incrementally.
			if initTotal, errInit := queryStoredContentBytes(tx); errInit == nil {
				requestLogContentBytes.Store(initTotal)
				total = initTotal
			} else {
				// Fallback to scan-based enforcement when we can't initialize the counter.
				deletedRows, errCleanup := cleanupOversizedLogContentQuerier(tx, maxBytes)
				if errCleanup != nil {
					return fmt.Errorf("usage: enforce content size cap: %w", errCleanup)
				}
				if deletedRows > 0 {
					requestLogContentBytes.Store(-1)
					triggerRequestLogCompaction()
				}
				return nil
			}
		}

		// Enforce cap without per-request full table SUM() scans.
		trimmedBytes, errTrim := cleanupOversizedLogContentQuerierWithTotal(tx, total, maxBytes)
		if errTrim != nil {
			return fmt.Errorf("usage: enforce content size cap: %w", errTrim)
		}
		if trimmedBytes > 0 {
			requestLogContentBytes.Add(-trimmedBytes)
			triggerRequestLogCompaction()
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
	if db == nil || !RequestLogBodyStorageEnabled() {
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

	// 迁移批处理是 DB 维护任务，不绑定任意请求生命周期。
	// 这里显式使用根 context，让事务仅受数据库自身错误/关闭影响。
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return 0, fmt.Errorf("usage: begin legacy migration tx: %w", err)
	}

	for _, row := range batch {
		timestamp, errParse := time.Parse(time.RFC3339Nano, row.Timestamp)
		if errParse != nil {
			timestamp = time.Now().UTC()
		}

		shouldKeep := RequestLogBodyStorageEnabled() && withinContentRetention(timestamp)
		if shouldKeep {
			if errStore := insertLogContentTx(tx, row.ID, timestamp, row.InputContent, row.OutputContent, ""); errStore != nil {
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
	cutoff := time.Now().UTC().AddDate(0, 0, -currentRequestLogStorageConfig().ContentRetentionDays)
	return !timestamp.Before(cutoff)
}

func cleanupExpiredLogContent(db *sql.DB) (int64, error) {
	if db == nil || contentRetentionUnlimited() {
		return 0, nil
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -currentRequestLogStorageConfig().ContentRetentionDays).Format(time.RFC3339Nano)
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

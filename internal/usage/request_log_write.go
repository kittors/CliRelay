package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

var errUsageProjectionBusy = errors.New("usage projection busy")

func isRetryableUsageWriteErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "deadlock") ||
		strings.Contains(msg, "could not serialize") ||
		strings.Contains(msg, "serialization failure") ||
		strings.Contains(msg, "lock wait timeout") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database is locked")
}

func insertLogIdentityOnce(
	db *sql.DB,
	tenantID, apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, thinkingLevel,
	source, channelName, authIndex, endUserID string,
	failed, streaming bool,
	timestamp time.Time, latencyMs, firstTokenMs int64,
	tokens TokenStats, cost float64,
	inputContent, outputContent, detailContent string,
	shouldStoreContent bool,
) error {
	// 插入 request log 的事务由 usage 存储层统一拥有，不从外部 HTTP 请求透传 context，
	// 以避免请求取消把已经选定要持久化的审计记录中断在半途。
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin insert tx: %w", err)
	}

	failedInt, streamingInt := 0, 0
	if failed {
		failedInt = 1
	}
	if streaming {
		streamingInt = 1
	}

	insertSQL := `INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, auth_subject_id, api_key_name, model, thinking_level, upstream_model, vision_fallback_model, source, channel_name, auth_index,
		 failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertArgs := []any{
		tenantID, timestamp.UTC().Format(time.RFC3339Nano),
		apiKey, apiKeyID, authSubjectID, apiKeyName, model, thinkingLevel, upstreamModel, visionFallbackModel, source, channelName, authIndex,
		failedInt, streamingInt, latencyMs, firstTokenMs,
		tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens,
		tokens.CachedTokens, tokens.TotalTokens, cost,
	}

	if shouldStoreContent {
		var logID int64
		if usageDriver == "postgres" {
			if err := tx.QueryRow(insertSQL+" RETURNING id", insertArgs...).Scan(&logID); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("insert log: %w", err)
			}
		} else {
			result, err := tx.Exec(insertSQL, insertArgs...)
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("insert log: %w", err)
			}
			logID, err = result.LastInsertId()
			if err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("resolve inserted log id: %w", err)
			}
		}
		if errStore := insertLogContentTenantTx(tx, tenantID, logID, timestamp, inputContent, outputContent, detailContent, failed); errStore != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert log content: %w", errStore)
		}
	} else if _, err := tx.Exec(insertSQL, insertArgs...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insert log: %w", err)
	}

	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("commit log insert: %w", errCommit)
	}

	projectRollupAfterLogInsert(db, rollupEvent{
		TenantID:      tenantID,
		APIKeyID:      apiKeyID,
		EndUserID:     endUserID,
		AuthSubjectID: authSubjectID,
		Model:         model,
		Source:        source,
		ChannelName:   channelName,
		Failed:        failed,
		Streaming:     streaming,
		LatencyMs:     latencyMs,
		FirstTokenMs:  firstTokenMs,
		Tokens:        tokens,
		Cost:          cost,
		At:            timestamp,
	})
	return nil
}

func projectRollupAfterLogInsert(db *sql.DB, ev rollupEvent) {
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * time.Millisecond)
		}
		if err := projectRollupOnce(db, ev); err != nil {
			if errors.Is(err, errUsageProjectionBusy) || isRetryableUsageWriteErr(err) {
				continue
			}
			log.Errorf("usage: project rollup after log insert: %v", err)
			return
		}
		return
	}
	log.Errorf("usage: project rollup after log insert: retries exhausted")
}

func projectRollupOnce(db *sql.DB, ev rollupEvent) error {
	if db == nil {
		return nil
	}
	if !usageProjectionMu.TryRLock() {
		return errUsageProjectionBusy
	}
	defer usageProjectionMu.RUnlock()

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin rollup tx: %w", err)
	}
	if usageDriver == "postgres" {
		if _, err := tx.Exec(`SET LOCAL lock_timeout = '1500ms'`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set rollup lock timeout: %w", err)
		}
		if _, err := tx.Exec(`SET LOCAL statement_timeout = '3000ms'`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set rollup statement timeout: %w", err)
		}
	}
	return commitLogWithProjections(tx, ev)
}

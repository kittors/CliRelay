package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// RequestLogEntry carries one request_logs row from the runtime down to the
// INSERT.
//
// It replaces a positional argument list that had grown past twenty parameters
// and a chain of InsertLogWithDetails*Identity*Subject*Upstream*Vision*Streaming
// wrappers whose names encoded which subset of fields each caller filled in.
// New columns now go here instead of adding another wrapper and another word to
// that name. The legacy wrappers are retained below for existing callers.
type RequestLogEntry struct {
	// TrustedTenantID is set only by authenticated internal execution paths;
	// when empty the tenant is resolved from the API key.
	TrustedTenantID string
	APIKey          string
	APIKeyID        string
	AuthSubjectID   string
	APIKeyName      string

	// Model is the model this request is logged and billed under, always the
	// clean request-time name.
	Model string
	// UpstreamModel is what we actually sent upstream when a mapping applied.
	UpstreamModel string
	// UpstreamResponseModel is what the upstream declared in its own response,
	// recorded for auditing only. See the executor's upstream response observer.
	UpstreamResponseModel string
	VisionFallbackModel   string
	ThinkingLevel         string

	Source      string
	ChannelName string
	AuthIndex   string

	Failed    bool
	Streaming bool

	Timestamp    time.Time
	LatencyMs    int64
	FirstTokenMs int64
	Tokens       TokenStats

	InputContent  string
	OutputContent string
	DetailContent string
}

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

func insertRequestLogOnce(
	db *sql.DB,
	tenantID, endUserID string,
	cost float64,
	shouldStoreContent bool,
	entry RequestLogEntry,
) error {
	// Shared projection lock before opening a DB tx so exclusive rebuilds never
	// leave writers holding connections while waiting on the mutex (pool deadlock).
	usageProjectionMu.RLock()
	defer usageProjectionMu.RUnlock()

	// 插入 request log 的事务由 usage 存储层统一拥有，不从外部 HTTP 请求透传 context，
	// 以避免请求取消把已经选定要持久化的审计记录中断在半途。
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin insert tx: %w", err)
	}

	failedInt, streamingInt := 0, 0
	if entry.Failed {
		failedInt = 1
	}
	if entry.Streaming {
		streamingInt = 1
	}

	insertSQL := `INSERT INTO request_logs
		(tenant_id, timestamp, api_key, api_key_id, auth_subject_id, api_key_name, model, thinking_level, upstream_model, upstream_response_model, vision_fallback_model, source, channel_name, auth_index,
		 failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertArgs := []any{
		tenantID, entry.Timestamp.UTC().Format(time.RFC3339Nano),
		entry.APIKey, entry.APIKeyID, entry.AuthSubjectID, entry.APIKeyName, entry.Model, entry.ThinkingLevel,
		entry.UpstreamModel, entry.UpstreamResponseModel, entry.VisionFallbackModel, entry.Source, entry.ChannelName, entry.AuthIndex,
		failedInt, streamingInt, entry.LatencyMs, entry.FirstTokenMs,
		entry.Tokens.InputTokens, entry.Tokens.OutputTokens, entry.Tokens.ReasoningTokens,
		entry.Tokens.CachedTokens, entry.Tokens.TotalTokens, cost,
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
		if errStore := insertLogContentTenantTx(tx, tenantID, logID, entry.Timestamp, entry.InputContent, entry.OutputContent, entry.DetailContent, entry.Failed); errStore != nil {
			_ = tx.Rollback()
			return fmt.Errorf("insert log content: %w", errStore)
		}
	} else if _, err := tx.Exec(insertSQL, insertArgs...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("insert log: %w", err)
	}

	if errCommit := commitLogWithProjections(tx, rollupEvent{
		TenantID:      tenantID,
		APIKeyID:      entry.APIKeyID,
		EndUserID:     endUserID,
		AuthSubjectID: entry.AuthSubjectID,
		Model:         entry.Model,
		Source:        entry.Source,
		ChannelName:   entry.ChannelName,
		Failed:        entry.Failed,
		Streaming:     entry.Streaming,
		LatencyMs:     entry.LatencyMs,
		FirstTokenMs:  entry.FirstTokenMs,
		Tokens:        entry.Tokens,
		Cost:          cost,
		At:            entry.Timestamp,
	}); errCommit != nil {
		return fmt.Errorf("commit log insert: %w", errCommit)
	}
	return nil
}

// InsertRequestLog writes a single request log entry into the runtime database.
// It is safe to call concurrently.
func InsertRequestLog(entry RequestLogEntry) {
	db := getDB()
	if db == nil {
		return
	}

	tenantID := resolveRequestLogTenantID(entry.TrustedTenantID, entry.APIKey)

	// Calculate cost from the trusted execution tenant or API key tenant catalog.
	// Cost always follows Model, never the model the upstream echoed back.
	cost := CalculateCostV2ForTenant(tenantID, entry.Model, entry.Tokens)

	entry.APIKeyID = strings.TrimSpace(entry.APIKeyID)
	entry.AuthSubjectID = strings.TrimSpace(entry.AuthSubjectID)
	entry.APIKeyName = strings.TrimSpace(entry.APIKeyName)
	entry.UpstreamModel = strings.TrimSpace(entry.UpstreamModel)
	entry.UpstreamResponseModel = strings.TrimSpace(entry.UpstreamResponseModel)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
	entry.ThinkingLevel = strings.TrimSpace(entry.ThinkingLevel)

	// Resolve identity before opening the write tx: a single-writer store + maintenance
	// would deadlock if we query api_keys while this connection already holds a tx.
	endUserID := ""
	if row := GetAPIKey(entry.APIKey); row != nil {
		if entry.APIKeyID == "" {
			entry.APIKeyID = strings.TrimSpace(row.ID)
		}
		if name := strings.TrimSpace(row.Name); name != "" {
			entry.APIKeyName = name
		} else if entry.APIKeyName == "" {
			entry.APIKeyName = strings.TrimSpace(row.Name)
		}
		endUserID = strings.TrimSpace(row.EndUserID)
	}

	// Failed requests always keep a compact error payload in output_content so the
	// management UI error modal can show the upstream failure even when full body
	// storage is disabled. Successful request/response bodies still follow the
	// store-content toggle.
	shouldStoreContent := entry.DetailContent != "" ||
		(RequestLogBodyStorageEnabled() && (entry.InputContent != "" || entry.OutputContent != "")) ||
		(entry.Failed && strings.TrimSpace(entry.OutputContent) != "")

	// Retry on Postgres rollup UPSERT deadlocks under concurrent same-key traffic.
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*attempt) * time.Millisecond)
		}
		lastErr = insertRequestLogOnce(db, tenantID, endUserID, cost, shouldStoreContent, entry)
		if lastErr == nil {
			break
		}
		if !isRetryableUsageWriteErr(lastErr) {
			log.Errorf("usage: insert log: %v", lastErr)
			return
		}
	}
	if lastErr != nil {
		log.Errorf("usage: insert log after retries: %v", lastErr)
		return
	}

	// Notify TPM tracker about token usage
	if tokenUsageCallback != nil && entry.Tokens.TotalTokens > 0 {
		tokenUsageCallback(entry.APIKey, entry.Tokens.TotalTokens)
	}
}

func isStreamingRequestContent(content string) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return false
	}
	return payload.Stream
}

// The InsertLogWithDetails* family below predates RequestLogEntry; each name
// encodes which subset of columns that caller fills in. They are kept as thin
// delegates so existing callers and tests keep working — new columns belong on
// RequestLogEntry, not in another wrapper.

// InsertLog writes a single request log entry into the runtime database.
// It is safe to call concurrently.
func InsertLog(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyName: apiKeyName, Model: model,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent,
	})
}

func InsertLogWithDetails(apiKey, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyName: apiKeyName, Model: model,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

func InsertLogWithDetailsIdentity(apiKey, apiKeyID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyID: apiKeyID, APIKeyName: apiKeyName, Model: model,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

func InsertLogWithDetailsIdentitySubject(apiKey, apiKeyID, authSubjectID, apiKeyName, model, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyID: apiKeyID, AuthSubjectID: authSubjectID, APIKeyName: apiKeyName, Model: model,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

func InsertLogWithDetailsIdentitySubjectUpstream(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyID: apiKeyID, AuthSubjectID: authSubjectID, APIKeyName: apiKeyName,
		Model: model, UpstreamModel: upstreamModel,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

func InsertLogWithDetailsIdentitySubjectUpstreamVision(apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string) {
	InsertRequestLog(RequestLogEntry{
		APIKey: apiKey, APIKeyID: apiKeyID, AuthSubjectID: authSubjectID, APIKeyName: apiKeyName,
		Model: model, UpstreamModel: upstreamModel, VisionFallbackModel: visionFallbackModel,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: isStreamingRequestContent(inputContent),
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

// InsertLogWithDetailsIdentitySubjectUpstreamVisionStreaming persists an
// explicit streaming classification even when request body storage is disabled.
func InsertLogWithDetailsIdentitySubjectUpstreamVisionStreaming(trustedTenantID, apiKey, apiKeyID, authSubjectID, apiKeyName, model, upstreamModel, visionFallbackModel, thinkingLevel, source, channelName, authIndex string,
	failed bool, timestamp time.Time, latencyMs, firstTokenMs int64, tokens TokenStats,
	inputContent, outputContent, detailContent string, streaming bool) {
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: trustedTenantID,
		APIKey:          apiKey, APIKeyID: apiKeyID, AuthSubjectID: authSubjectID, APIKeyName: apiKeyName,
		Model: model, UpstreamModel: upstreamModel, VisionFallbackModel: visionFallbackModel, ThinkingLevel: thinkingLevel,
		Source: source, ChannelName: channelName, AuthIndex: authIndex,
		Failed: failed, Streaming: streaming,
		Timestamp: timestamp, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs, Tokens: tokens,
		InputContent: inputContent, OutputContent: outputContent, DetailContent: detailContent,
	})
}

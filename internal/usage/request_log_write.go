package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
	// IdempotencyKey makes the write exactly-once: a retry or spool replay of
	// an entry whose earlier attempt already committed is skipped. Records from
	// the usage queue carry the key assigned when they were published;
	// InsertRequestLog assigns one to entries that arrive without it.
	IdempotencyKey string

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

// isRetryableUsageWriteErr reports lock contention between concurrent writers,
// which a quick retry resolves.
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

// insertRequestLogRowTx inserts the request_logs row and, when the plan keeps
// content, its compressed body row.
func insertRequestLogRowTx(tx usageWriteTx, plan requestLogWritePlan) error {
	entry := plan.entry
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
		plan.tenantID, entry.Timestamp.UTC().Format(time.RFC3339Nano),
		entry.APIKey, entry.APIKeyID, entry.AuthSubjectID, entry.APIKeyName, entry.Model, entry.ThinkingLevel,
		entry.UpstreamModel, entry.UpstreamResponseModel, entry.VisionFallbackModel, entry.Source, entry.ChannelName, entry.AuthIndex,
		failedInt, streamingInt, entry.LatencyMs, entry.FirstTokenMs,
		entry.Tokens.InputTokens, entry.Tokens.OutputTokens, entry.Tokens.ReasoningTokens,
		entry.Tokens.CachedTokens, entry.Tokens.TotalTokens, plan.cost,
	}

	if !plan.storeContent {
		if _, err := tx.Exec(insertSQL, insertArgs...); err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
		return nil
	}
	var logID int64
	if usageDriver == "postgres" {
		if err := tx.QueryRow(insertSQL+" RETURNING id", insertArgs...).Scan(&logID); err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
	} else {
		result, err := tx.Exec(insertSQL, insertArgs...)
		if err != nil {
			return fmt.Errorf("insert log: %w", err)
		}
		logID, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("resolve inserted log id: %w", err)
		}
	}
	if err := insertLogContentTenantTx(tx, plan.tenantID, logID, entry.Timestamp, entry.InputContent, entry.OutputContent, entry.DetailContent, entry.Failed); err != nil {
		return fmt.Errorf("insert log content: %w", err)
	}
	return nil
}

// normalizeRequestLogEntry trims identity fields and makes sure the entry
// carries an idempotency key before its first attempt, so every retry and
// replay of it shares that key.
func normalizeRequestLogEntry(entry RequestLogEntry) RequestLogEntry {
	entry.IdempotencyKey = strings.TrimSpace(entry.IdempotencyKey)
	if entry.IdempotencyKey == "" {
		entry.IdempotencyKey = coreusage.NewIdempotencyKey()
	}
	entry.APIKeyID = strings.TrimSpace(entry.APIKeyID)
	entry.AuthSubjectID = strings.TrimSpace(entry.AuthSubjectID)
	entry.APIKeyName = strings.TrimSpace(entry.APIKeyName)
	entry.UpstreamModel = strings.TrimSpace(entry.UpstreamModel)
	entry.UpstreamResponseModel = strings.TrimSpace(entry.UpstreamResponseModel)
	entry.VisionFallbackModel = strings.TrimSpace(entry.VisionFallbackModel)
	entry.ThinkingLevel = strings.TrimSpace(entry.ThinkingLevel)
	return entry
}

// InsertRequestLog writes a single request log entry into the runtime database.
// It is safe to call concurrently.
//
// With the local spool running, a database that stays unreachable through the
// retry budget no longer costs the record: it is appended to the spool and
// replayed once the database is back. While such an outage is known, new
// entries go straight to the spool so the usage worker keeps pace with
// traffic instead of spending the retry budget on every record.
func InsertRequestLog(entry RequestLogEntry) {
	entry = normalizeRequestLogEntry(entry)
	spool := activeUsageSpool()
	if spool == nil && getDB() == nil {
		return
	}
	if spool != nil && usageDBHealth.down() {
		spoolLiveRequestLog(spool, entry, usageSpoolReasonDBUnavailable, time.Time{}, nil)
		return
	}
	result, outcome, err := writeRequestLogWithRetry(entry, liveRequestLogRetryBudget(spool))
	switch outcome {
	case requestLogWriteCommitted:
		usageDBHealth.markUp()
		if spool != nil && result.duplicateNeedsRecheck(time.Now()) {
			// The key found may be our own commit that a failover is about to
			// discard; the replayer checks it again after the settle window.
			spoolLiveRequestLog(spool, entry, usageSpoolReasonVerifyCommit, result.commitUncertainAt, nil)
			return
		}
		notifyTokenUsage(entry, result.endUserID)
	case requestLogWriteTransient:
		if spool == nil {
			log.Errorf("usage: insert log after retries: %v", err)
			return
		}
		usageDBHealth.markDown(err)
		spoolLiveRequestLog(spool, entry, usageSpoolReasonWriteFailed, result.commitUncertainAt, err)
	default:
		log.Errorf("usage: insert log: %v", err)
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

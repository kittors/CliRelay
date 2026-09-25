package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// usageSpoolRecordVersion is the on-disk format version of a spooled record.
const usageSpoolRecordVersion = 1

// usageSpoolReason records why an entry went to the spool, for diagnostics.
type usageSpoolReason string

const (
	// usageSpoolReasonWriteFailed: the worker's retries ran out.
	usageSpoolReasonWriteFailed usageSpoolReason = "write_failed"
	// usageSpoolReasonDBUnavailable: an outage was already known, so the
	// worker skipped the database.
	usageSpoolReasonDBUnavailable usageSpoolReason = "db_unavailable"
	// usageSpoolReasonQueueFull: the usage queue was full and the publishing
	// request must not wait for it.
	usageSpoolReasonQueueFull usageSpoolReason = "queue_full"
	// usageSpoolReasonShutdown: published after the usage queue stopped.
	usageSpoolReasonShutdown usageSpoolReason = "shutdown"
	// usageSpoolReasonVerifyCommit: the key was found already stored right
	// after a lost COMMIT reply; the replayer confirms it once the commit can
	// no longer be discarded by a failover.
	usageSpoolReasonVerifyCommit usageSpoolReason = "verify_commit"
)

var errUsageSpoolRecordInvalid = errors.New("usage: invalid spooled request log record")

// usageSpoolRecord is one pending request log write on disk, one JSON object
// per line. Field names are the file format: a record spooled by one release
// must replay on the next, so fields are only ever added, never renamed.
//
// The record keeps the raw entry, not a resolved one. Tenant, key owner and
// cost are resolved when the record is finally written, because resolving
// them while the database is unreachable would bake in the fallbacks.
type usageSpoolRecord struct {
	Version   int       `json:"v"`
	Key       string    `json:"key"`
	SpooledAt time.Time `json:"spooled_at"`
	Reason    string    `json:"reason,omitempty"`
	// CommitUncertainAt is when an attempt of this record last lost its
	// COMMIT reply; a duplicate found before the settle window has passed is
	// checked again rather than trusted.
	CommitUncertainAt time.Time `json:"commit_uncertain_at,omitzero"`

	TrustedTenantID       string           `json:"trusted_tenant_id,omitempty"`
	APIKey                string           `json:"api_key,omitempty"`
	APIKeyID              string           `json:"api_key_id,omitempty"`
	AuthSubjectID         string           `json:"auth_subject_id,omitempty"`
	APIKeyName            string           `json:"api_key_name,omitempty"`
	Model                 string           `json:"model,omitempty"`
	UpstreamModel         string           `json:"upstream_model,omitempty"`
	UpstreamResponseModel string           `json:"upstream_response_model,omitempty"`
	VisionFallbackModel   string           `json:"vision_fallback_model,omitempty"`
	ThinkingLevel         string           `json:"thinking_level,omitempty"`
	Source                string           `json:"source,omitempty"`
	ChannelName           string           `json:"channel_name,omitempty"`
	AuthIndex             string           `json:"auth_index,omitempty"`
	Failed                bool             `json:"failed,omitempty"`
	Streaming             bool             `json:"streaming,omitempty"`
	Timestamp             time.Time        `json:"timestamp"`
	LatencyMs             int64            `json:"latency_ms,omitempty"`
	FirstTokenMs          int64            `json:"first_token_ms,omitempty"`
	Tokens                usageSpoolTokens `json:"tokens"`

	// Bodies are zstd-compressed (base64 in JSON) and present only when the
	// body-storage policy would store them anyway.
	InputContent  []byte `json:"input_zst,omitempty"`
	OutputContent []byte `json:"output_zst,omitempty"`
	DetailContent []byte `json:"detail_zst,omitempty"`
}

// usageSpoolTokens spells out every TokenStats field: TokenStats does not
// serialise CacheReadIncludedInInput, and cost depends on it.
type usageSpoolTokens struct {
	Input                    int64 `json:"input,omitempty"`
	Output                   int64 `json:"output,omitempty"`
	Reasoning                int64 `json:"reasoning,omitempty"`
	Cached                   int64 `json:"cached,omitempty"`
	Total                    int64 `json:"total,omitempty"`
	CacheRead                int64 `json:"cache_read,omitempty"`
	CacheWrite               int64 `json:"cache_write,omitempty"`
	CacheReadIncludedInInput bool  `json:"cache_read_included_in_input,omitempty"`
}

// encodeUsageSpoolRecord renders entry as one newline-terminated spool line.
//
// Bodies follow the same policy the insert would apply: with body storage off
// the request body is dropped and only the compact failure payload and the
// body-free detail are kept, so the spool never writes to disk what the
// database would have refused to keep.
func encodeUsageSpoolRecord(entry RequestLogEntry, reason usageSpoolReason, commitUncertainAt, now time.Time) ([]byte, error) {
	record := usageSpoolRecord{
		Version:               usageSpoolRecordVersion,
		Key:                   entry.IdempotencyKey,
		SpooledAt:             now.UTC(),
		Reason:                string(reason),
		CommitUncertainAt:     commitUncertainAt.UTC(),
		TrustedTenantID:       entry.TrustedTenantID,
		APIKey:                entry.APIKey,
		APIKeyID:              entry.APIKeyID,
		AuthSubjectID:         entry.AuthSubjectID,
		APIKeyName:            entry.APIKeyName,
		Model:                 entry.Model,
		UpstreamModel:         entry.UpstreamModel,
		UpstreamResponseModel: entry.UpstreamResponseModel,
		VisionFallbackModel:   entry.VisionFallbackModel,
		ThinkingLevel:         entry.ThinkingLevel,
		Source:                entry.Source,
		ChannelName:           entry.ChannelName,
		AuthIndex:             entry.AuthIndex,
		Failed:                entry.Failed,
		Streaming:             entry.Streaming,
		Timestamp:             entry.Timestamp.UTC(),
		LatencyMs:             entry.LatencyMs,
		FirstTokenMs:          entry.FirstTokenMs,
		Tokens: usageSpoolTokens{
			Input:                    entry.Tokens.InputTokens,
			Output:                   entry.Tokens.OutputTokens,
			Reasoning:                entry.Tokens.ReasoningTokens,
			Cached:                   entry.Tokens.CachedTokens,
			Total:                    entry.Tokens.TotalTokens,
			CacheRead:                entry.Tokens.CacheReadTokens,
			CacheWrite:               entry.Tokens.CacheWriteTokens,
			CacheReadIncludedInInput: entry.Tokens.CacheReadIncludedInInput,
		},
	}
	if strings.TrimSpace(record.Key) == "" {
		return nil, fmt.Errorf("%w: missing idempotency key", errUsageSpoolRecordInvalid)
	}
	if requestLogShouldStoreContent(entry) {
		if err := record.setContent(entry); err != nil {
			return nil, err
		}
	}
	line, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("usage: encode spooled request log: %w", err)
	}
	return append(line, '\n'), nil
}

func (r *usageSpoolRecord) setContent(entry RequestLogEntry) error {
	input, output, detail := entry.InputContent, entry.OutputContent, entry.DetailContent
	if !RequestLogBodyStorageEnabled() {
		input = ""
		if entry.Failed {
			output = compactFailedOutputContent(output)
		} else {
			output = ""
		}
		detail = stripStoredRequestDetailBodies(detail)
	}
	var err error
	if r.InputContent, err = compressLogContent(input); err != nil {
		return err
	}
	if r.OutputContent, err = compressLogContent(output); err != nil {
		return err
	}
	if r.DetailContent, err = compressLogContent(detail); err != nil {
		return err
	}
	// The insert skips a body above the stored-content cap, so the spool does
	// not keep one either; the usage numbers are what must survive.
	if maxBytes := maxLogContentBytes(); maxBytes > 0 &&
		int64(len(r.InputContent)+len(r.OutputContent)+len(r.DetailContent)) > maxBytes {
		r.InputContent, r.OutputContent, r.DetailContent = nil, nil, nil
	}
	return nil
}

// decodeUsageSpoolRecord parses one spool line back into the entry it came from.
func decodeUsageSpoolRecord(line []byte) (RequestLogEntry, usageSpoolRecord, error) {
	var record usageSpoolRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return RequestLogEntry{}, record, fmt.Errorf("%w: %v", errUsageSpoolRecordInvalid, err)
	}
	if record.Version != usageSpoolRecordVersion {
		return RequestLogEntry{}, record, fmt.Errorf("%w: unsupported version %d", errUsageSpoolRecordInvalid, record.Version)
	}
	if strings.TrimSpace(record.Key) == "" {
		return RequestLogEntry{}, record, fmt.Errorf("%w: missing idempotency key", errUsageSpoolRecordInvalid)
	}
	entry := RequestLogEntry{
		IdempotencyKey:        record.Key,
		TrustedTenantID:       record.TrustedTenantID,
		APIKey:                record.APIKey,
		APIKeyID:              record.APIKeyID,
		AuthSubjectID:         record.AuthSubjectID,
		APIKeyName:            record.APIKeyName,
		Model:                 record.Model,
		UpstreamModel:         record.UpstreamModel,
		UpstreamResponseModel: record.UpstreamResponseModel,
		VisionFallbackModel:   record.VisionFallbackModel,
		ThinkingLevel:         record.ThinkingLevel,
		Source:                record.Source,
		ChannelName:           record.ChannelName,
		AuthIndex:             record.AuthIndex,
		Failed:                record.Failed,
		Streaming:             record.Streaming,
		Timestamp:             record.Timestamp,
		LatencyMs:             record.LatencyMs,
		FirstTokenMs:          record.FirstTokenMs,
		Tokens: TokenStats{
			InputTokens:              record.Tokens.Input,
			OutputTokens:             record.Tokens.Output,
			ReasoningTokens:          record.Tokens.Reasoning,
			CachedTokens:             record.Tokens.Cached,
			TotalTokens:              record.Tokens.Total,
			CacheReadTokens:          record.Tokens.CacheRead,
			CacheWriteTokens:         record.Tokens.CacheWrite,
			CacheReadIncludedInInput: record.Tokens.CacheReadIncludedInInput,
		},
	}
	var err error
	if entry.InputContent, err = decompressLogContent(requestLogContentCompression, record.InputContent); err != nil {
		return RequestLogEntry{}, record, err
	}
	if entry.OutputContent, err = decompressLogContent(requestLogContentCompression, record.OutputContent); err != nil {
		return RequestLogEntry{}, record, err
	}
	if entry.DetailContent, err = decompressLogContent(requestLogContentCompression, record.DetailContent); err != nil {
		return RequestLogEntry{}, record, err
	}
	return entry, record, nil
}

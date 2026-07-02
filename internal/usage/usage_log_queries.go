package usage

import (
	"fmt"
	"time"
)

// QueryLogRowByID returns a single request log row by primary key.
func QueryLogRowByID(id int64) (LogRow, error) {
	db := getReadDB()
	if db == nil {
		return LogRow{}, fmt.Errorf("usage: database not initialised")
	}

	var row LogRow
	var ts string
	var failedInt, streamingInt, hasContentInt int
	err := db.QueryRow(
		"SELECT id, timestamp, api_key, api_key_name, model, source, channel_name, auth_index, "+
			"failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, "+
			"cost, "+
			"(CASE WHEN EXISTS (SELECT 1 FROM request_log_content content WHERE content.log_id = request_logs.id) "+
			"OR length(input_content) > 0 OR length(output_content) > 0 THEN 1 ELSE 0 END) as has_content "+
			"FROM request_logs WHERE id = ?",
		id,
	).Scan(
		&row.ID, &ts, &row.APIKey, &row.APIKeyName, &row.Model, &row.Source, &row.ChannelName,
		&row.AuthIndex, &failedInt, &streamingInt, &row.LatencyMs, &row.FirstTokenMs,
		&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
		&row.CachedTokens, &row.TotalTokens, &row.Cost, &hasContentInt,
	)
	if err != nil {
		return LogRow{}, fmt.Errorf("usage: query log row: %w", err)
	}
	row.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
	row.Failed = failedInt != 0
	row.Streaming = streamingInt != 0
	row.HasContent = hasContentInt != 0
	rows := []LogRow{row}
	hydrateStreamingFromContent(db, rows)
	row = rows[0]
	return row, nil
}

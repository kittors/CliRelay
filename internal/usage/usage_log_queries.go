package usage

import "fmt"

// QueryLogRowByID returns a single request log row by primary key.
func QueryLogRowByID(id int64) (LogRow, error) {
	return QueryLogRowByIDForTenant(systemTenantID, id)
}

func QueryLogRowByIDForTenant(tenantID string, id int64) (LogRow, error) {
	tenantID = normalizeTenantID(tenantID)
	db := getReadDB()
	if db == nil {
		return LogRow{}, fmt.Errorf("usage: database not initialised")
	}

	var row LogRow
	var ts storedTime
	var failedInt, streamingInt, hasContentInt int
	err := db.QueryRow(
		"SELECT id, timestamp, api_key, api_key_name, model, thinking_level, source, channel_name, auth_index, "+
			"failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, "+
			"cost, "+
			"(CASE WHEN EXISTS (SELECT 1 FROM request_log_content content WHERE content.tenant_id = request_logs.tenant_id AND content.log_id = request_logs.id) "+
			"OR length(input_content) > 0 OR length(output_content) > 0 THEN 1 ELSE 0 END) as has_content "+
			"FROM request_logs WHERE tenant_id = ? AND id = ?",
		tenantID, id,
	).Scan(
		&row.ID, &ts, &row.APIKey, &row.APIKeyName, &row.Model, &row.ThinkingLevel, &row.Source, &row.ChannelName,
		&row.AuthIndex, &failedInt, &streamingInt, &row.LatencyMs, &row.FirstTokenMs,
		&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
		&row.CachedTokens, &row.TotalTokens, &row.Cost, &hasContentInt,
	)
	if err != nil {
		return LogRow{}, fmt.Errorf("usage: query log row: %w", err)
	}
	if ts.Valid {
		row.Timestamp = ts.Time
	}
	row.Failed = failedInt != 0
	row.Streaming = streamingInt != 0
	row.HasContent = hasContentInt != 0
	// Rows written before masking may still hold the secret; never hand it out.
	row.APIKey = requestLogAPIKeyForDisplay(row.APIKey)
	rows := []LogRow{row}
	hydrateStreamingFromContent(db, tenantID, rows)
	row = rows[0]
	return row, nil
}

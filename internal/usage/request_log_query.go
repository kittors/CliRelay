package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// QueryLogs returns a paginated, filtered list of log entries.
func QueryLogs(params LogQueryParams) (LogQueryResult, error) {
	// Normalise parameters
	if params.Page < 1 {
		params.Page = 1
	}
	if params.Size < 1 {
		params.Size = 50
	}
	if params.Size > 500 {
		params.Size = 500
	}
	if params.Days < 1 {
		params.Days = 7
	}

	// Read path: use the dedicated read-only pool so management log queries do not
	// block on the single writer or maintenance ops (wal_checkpoint/VACUUM).
	db := getReadDB()
	if db == nil {
		// Never return nil slices in JSON responses (nil slice => null in JSON).
		return LogQueryResult{
			Items: make([]LogRow, 0),
			Total: 0,
			Page:  params.Page,
			Size:  params.Size,
		}, nil
	}

	where, args := buildWhereClause(params)

	// Count total
	var total int64
	countSQL := "SELECT COUNT(*) FROM request_logs" + where
	if err := db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		log.Warnf("usage: count query failed: %v", err)
		return LogQueryResult{}, fmt.Errorf("usage: count query: %w", err)
	}

	// Fetch page
	offset := (params.Page - 1) * params.Size
	querySQL := "SELECT id, timestamp, api_key, api_key_id, api_key_name, model, upstream_model, vision_fallback_model, source, channel_name, auth_index, auth_subject_id, " +
		"failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, " +
		"cost, " +
		"(CASE WHEN EXISTS (SELECT 1 FROM request_log_content content WHERE content.tenant_id = request_logs.tenant_id AND content.log_id = request_logs.id) " +
		"OR length(input_content) > 0 OR length(output_content) > 0 THEN 1 ELSE 0 END) as has_content " +
		"FROM request_logs" + where +
		" ORDER BY timestamp DESC LIMIT ? OFFSET ?"
	queryArgs := append(args, params.Size, offset)

	rows, err := db.Query(querySQL, queryArgs...)
	if err != nil {
		log.Warnf("usage: query logs failed: %v", err)
		return LogQueryResult{}, fmt.Errorf("usage: query logs: %w", err)
	}
	defer rows.Close()

	items := make([]LogRow, 0, params.Size)
	for rows.Next() {
		var row LogRow
		var ts storedTime
		var failedInt, streamingInt, hasContentInt int
		if err := rows.Scan(
			&row.ID, &ts, &row.APIKey, &row.APIKeyID, &row.APIKeyName, &row.Model, &row.UpstreamModel, &row.VisionFallbackModel, &row.Source, &row.ChannelName,
			&row.AuthIndex, &row.AuthSubjectID, &failedInt, &streamingInt, &row.LatencyMs, &row.FirstTokenMs,
			&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
			&row.CachedTokens, &row.TotalTokens, &row.Cost, &hasContentInt,
		); err != nil {
			log.Warnf("usage: scan log row failed: %v", err)
			return LogQueryResult{}, fmt.Errorf("usage: scan row: %w", err)
		}
		if ts.Valid {
			row.Timestamp = ts.Time
		}
		row.Failed = failedInt != 0
		row.Streaming = streamingInt != 0
		row.HasContent = hasContentInt != 0
		items = append(items, row)
	}
	hydrateStreamingFromContent(db, params.TenantID, items)
	hydrateAPIKeyDisplayNames(params.TenantID, items)

	return LogQueryResult{
		Items: items,
		Total: total,
		Page:  params.Page,
		Size:  params.Size,
	}, nil
}

// hydrateAPIKeyDisplayNames keeps account and credential identity separate.
func hydrateAPIKeyDisplayNames(tenantID string, items []LogRow) {
	if len(items) == 0 {
		return
	}
	byID := currentAPIKeyRowsByIDForTenant(tenantID)
	byKey := currentAPIKeyRowsByKeyForTenant(tenantID)
	for i := range items {
		var row *APIKeyRow
		if r, ok := byID[strings.TrimSpace(items[i].APIKeyID)]; ok {
			copy := r
			row = &copy
		} else if r, ok := byKey[strings.TrimSpace(items[i].APIKey)]; ok {
			copy := r
			row = &copy
		} else {
			// Fall back: resolve via secret lookup when tenant maps miss.
			if live := GetAPIKey(items[i].APIKey); live != nil {
				row = live
			}
		}
		if row != nil {
			items[i].APIKeyID = strings.TrimSpace(row.ID)
			items[i].APIKeyOwnName = strings.TrimSpace(row.Name)
			items[i].EndUserDisplayName = DisplayNameForEndUser(row.EndUserID)
			if items[i].EndUserDisplayName != "" {
				items[i].APIKeyName = items[i].EndUserDisplayName
			} else if items[i].APIKeyOwnName != "" {
				items[i].APIKeyName = items[i].APIKeyOwnName
			}
		} else {
			// Legacy rows have only one snapshot label. Preserve it as the key name;
			// no account identity is inferred without a trusted ownership row.
			items[i].APIKeyOwnName = strings.TrimSpace(items[i].APIKeyName)
		}
	}
}

func hydrateStreamingFromContent(db *sql.DB, tenantID string, items []LogRow) {
	if db == nil || len(items) == 0 {
		return
	}

	indexByID := make(map[int64]int, len(items))
	ids := make([]int64, 0, len(items))
	for i := range items {
		if items[i].Streaming {
			continue
		}
		indexByID[items[i].ID] = i
		ids = append(ids, items[i].ID)
	}
	if len(ids) == 0 {
		return
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, normalizeTenantID(tenantID))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(
		`SELECT logs.id, logs.input_content, content.compression, content.input_content
		FROM request_logs logs
		LEFT JOIN request_log_content content ON content.tenant_id = logs.tenant_id AND content.log_id = logs.id
		WHERE logs.tenant_id = ? AND logs.id IN (`+placeholders+`) AND (length(logs.input_content) > 0 OR length(content.input_content) > 0)`,
		args...,
	)
	if err != nil {
		log.Warnf("usage: query request log content for streaming flag: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var legacyInput string
		var compression sql.NullString
		var storedInput []byte
		if err := rows.Scan(&id, &legacyInput, &compression, &storedInput); err != nil {
			log.Warnf("usage: scan request log content for streaming flag: %v", err)
			continue
		}
		streaming := isStreamingRequestContent(legacyInput)
		if !streaming && len(storedInput) > 0 {
			compressionName := "zstd"
			if compression.Valid {
				compressionName = compression.String
			}
			input, err := decompressLogContent(compressionName, storedInput)
			if err != nil {
				log.Warnf("usage: decompress request log content for streaming flag: %v", err)
				continue
			}
			streaming = isStreamingRequestContent(input)
		}
		if index, ok := indexByID[id]; ok && streaming {
			items[index].Streaming = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("usage: iterate request log content for streaming flag: %v", err)
	}
}

// QueryFilters returns the distinct API keys and models within the time range.
func QueryFilters(days int) (FilterOptions, error) {
	return QueryFiltersForLogs(LogQueryParams{Days: days})
}

// QueryFiltersForLogs returns candidate filter values constrained by the
// current request-log query. Each facet ignores its own selection so users can
// widen that facet without resetting every filter first.
func QueryFiltersForLogs(params LogQueryParams) (FilterOptions, error) {
	db := getReadDB()
	if db == nil {
		// Ensure stable JSON shape: slices => [] (not null), maps => {} (not null).
		return FilterOptions{
			APIKeys:        make([]string, 0),
			APIKeyNames:    make(map[string]string),
			APIKeyCounts:   make(map[string]int64),
			APIKeyIDs:      make([]string, 0),
			APIKeyIDNames:  make(map[string]string),
			APIKeyIDCounts: make(map[string]int64),
			Models:         make([]string, 0),
			Channels:       make([]string, 0),
			ChannelOptions: make([]ChannelFilterOption, 0),
			Statuses:       make([]string, 0),
		}, nil
	}

	params = normalizeLogQueryParams(params)
	keys, keyNames, keyCounts, err := queryDistinctAPIKeys(db, params.withoutFacet("api_key"))
	if err != nil {
		log.Warnf("usage: query distinct api keys failed: %v", err)
		return FilterOptions{}, err
	}
	keyIDs, keyIDNames, keyIDCounts, err := queryDistinctAPIKeyIDs(db, params.withoutFacet("api_key_id"))
	if err != nil {
		log.Warnf("usage: query distinct api key ids failed: %v", err)
		return FilterOptions{}, err
	}
	models, err := queryDistinct(db, "model", params.withoutFacet("model"))
	if err != nil {
		log.Warnf("usage: query distinct models failed: %v", err)
		return FilterOptions{}, err
	}
	channelRows, err := queryDistinctChannelRows(db, params.withoutFacet("channel"))
	if err != nil {
		log.Warnf("usage: query distinct channels failed: %v", err)
		return FilterOptions{}, err
	}
	statuses, err := queryDistinctStatuses(db, params.withoutFacet("status"))
	if err != nil {
		log.Warnf("usage: query distinct statuses failed: %v", err)
		return FilterOptions{}, err
	}

	channelNames := make([]string, 0, len(channelRows))
	seenNames := make(map[string]struct{}, len(channelRows))
	for _, row := range channelRows {
		name := strings.TrimSpace(row.Label)
		if name == "" {
			name = strings.TrimSpace(row.Value)
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seenNames[key]; ok {
			continue
		}
		seenNames[key] = struct{}{}
		channelNames = append(channelNames, name)
	}

	return FilterOptions{
		APIKeys:        keys,
		APIKeyNames:    keyNames,
		APIKeyCounts:   keyCounts,
		APIKeyIDs:      keyIDs,
		APIKeyIDNames:  keyIDNames,
		APIKeyIDCounts: keyIDCounts,
		Models:         models,
		Channels:       channelNames,
		ChannelOptions: channelRows,
		Statuses:       statuses,
	}, nil
}

func (params LogQueryParams) withoutFacet(facet string) LogQueryParams {
	params.Page = 0
	params.Size = 0
	switch facet {
	case "api_key":
		params.APIKey = ""
		params.APIKeys = nil
		params.MatchNoAPIKeys = false
	case "api_key_id":
		params.APIKeyIDs = nil
		params.MatchNoAPIKeyIDs = false
	case "model":
		params.Model = ""
		params.Models = nil
		params.MatchNoModels = false
	case "channel":
		params.AuthSubjectIDs = nil
		params.AuthIndexes = nil
		params.ChannelNames = nil
		params.AuthIndexChannelNames = nil
		params.MatchNoChannels = false
	case "status":
		params.Status = ""
		params.Statuses = nil
		params.MatchNoStatuses = false
	}
	return params
}

// QueryStats returns aggregated statistics from usage rollup projection.
// Does not scan request_logs so detail retention cleanup cannot zero stats.
func QueryStats(params LogQueryParams) (LogStats, error) {
	if getReadDB() == nil {
		return LogStats{CacheRate: 0}, nil
	}
	// Explicit empty multi-selects match no rows; rollup does not model "empty model".
	if params.MatchNoAPIKeys || params.MatchNoAPIKeyIDs || params.MatchNoModels || params.MatchNoStatuses || params.MatchNoChannels {
		return LogStats{CacheRate: 0}, nil
	}
	stats, err := queryStatsFromRollup(params)
	if err != nil {
		log.Warnf("usage: stats query failed: %v", err)
		return LogStats{}, err
	}
	return stats, nil
}

// DeleteLogsByAPIKey removes all request_logs and request_log_content entries
// for the given API key. Returns the number of deleted log rows.
func DeleteLogsByAPIKey(apiKey string) (int64, error) {
	return DeleteLogsByAPIKeyForTenant(systemTenantID, apiKey)
}

func DeleteLogsByAPIKeyForTenant(tenantID, apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, fmt.Errorf("usage: database not initialised")
	}
	if apiKey == "" {
		return 0, fmt.Errorf("usage: empty api_key")
	}

	// Delete associated content rows first (FK cascade may handle this,
	// but be explicit to ensure cleanup even without FK enforcement).
	tenantID = normalizeTenantID(tenantID)
	clause, args := buildSingleAPIKeySelectorClauseForTenant(tenantID, apiKey)
	predicate := strings.TrimPrefix(clause, " WHERE ")
	args = append([]interface{}{tenantID}, args...)
	clause = " WHERE tenant_id = ? AND " + predicate
	_, _ = db.Exec(
		`DELETE FROM request_log_content WHERE tenant_id = ? AND log_id IN
		 (SELECT id FROM request_logs`+clause+`)`, append([]interface{}{tenantID}, args...)...)

	result, err := db.Exec("DELETE FROM request_logs"+clause, args...)
	if err != nil {
		return 0, fmt.Errorf("usage: delete logs by api_key: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows: %w", err)
	}
	if deleted > 0 {
		log.Infof("usage: deleted %d request log(s) for api_key=%s", deleted, apiKey)
	}
	return deleted, nil
}

func rowsAffected(result sql.Result) (int64, error) {
	if result == nil {
		return 0, nil
	}
	value, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return value, nil
}

func normalizeClearRequestLogsOptions(options ClearRequestLogsOptions) (ClearRequestLogsOptions, error) {
	options.TenantID = normalizeTenantID(options.TenantID)
	if options.ClearRequestRecords {
		options.ClearBodyContent = true
		options.ClearDetailContent = true
	}
	if !options.ClearBodyContent && !options.ClearDetailContent && !options.ClearRequestRecords {
		return ClearRequestLogsOptions{}, fmt.Errorf("usage: at least one cleanup option must be selected")
	}
	return options, nil
}

// ClearRequestLogs selectively clears request log bodies, details, and/or full request records.
func ClearRequestLogs(options ClearRequestLogsOptions) (ClearRequestLogsResult, error) {
	db := getDB()
	if db == nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: database not initialised")
	}
	return clearRequestLogs(db, options)
}

func clearRequestLogs(db *sql.DB, options ClearRequestLogsOptions) (ClearRequestLogsResult, error) {
	options, err := normalizeClearRequestLogsOptions(options)
	if err != nil {
		return ClearRequestLogsResult{}, err
	}

	tx, err := db.Begin()
	if err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: begin clear request logs: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var result ClearRequestLogsResult

	if options.ClearRequestRecords {
		contentResult, err := tx.Exec("DELETE FROM request_log_content WHERE tenant_id = ?", options.TenantID)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content: %w", err)
		}
		logResult, err := tx.Exec("DELETE FROM request_logs WHERE tenant_id = ?", options.TenantID)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_logs: %w", err)
		}

		result.DeletedContents, err = rowsAffected(contentResult)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: content affected rows: %w", err)
		}
		result.DeletedLogs, err = rowsAffected(logResult)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: log affected rows: %w", err)
		}
	} else {
		if options.ClearBodyContent {
			// Always drop request bodies. Preserve failed-request output_content so the
			// error-detail modal can still show the compact upstream error when
			// store-content is disabled (maintenance purge must not erase those).
			contentResult, err := tx.Exec(
				`UPDATE request_log_content
				 SET input_content = X'',
				     output_content = CASE
				       WHEN EXISTS (
				         SELECT 1 FROM request_logs logs
				         WHERE logs.tenant_id = request_log_content.tenant_id
				           AND logs.id = request_log_content.log_id
				           AND logs.failed = 1
				       ) THEN output_content
				       ELSE X''
				     END
				 WHERE tenant_id = ?
				   AND (length(input_content) > 0 OR length(output_content) > 0)`,
				options.TenantID,
			)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content bodies: %w", err)
			}
			result.ClearedBodyRows, err = rowsAffected(contentResult)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: body affected rows: %w", err)
			}

			legacyResult, err := tx.Exec(
				`UPDATE request_logs
				 SET input_content = '',
				     output_content = CASE WHEN failed = 1 THEN output_content ELSE '' END
				 WHERE tenant_id = ?
				   AND (length(input_content) > 0 OR length(output_content) > 0)`,
				options.TenantID,
			)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: clear legacy request log bodies: %w", err)
			}
			result.ClearedLegacyRows, err = rowsAffected(legacyResult)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: legacy affected rows: %w", err)
			}
		}

		if options.ClearDetailContent {
			detailResult, err := tx.Exec(
				"UPDATE request_log_content SET detail_content = X'', session_id = '' WHERE tenant_id = ? AND (length(detail_content) > 0 OR session_id <> '')",
				options.TenantID,
			)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content details: %w", err)
			}
			result.ClearedDetailRows, err = rowsAffected(detailResult)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: detail affected rows: %w", err)
			}
		}

		deleteEmptyRowsResult, err := tx.Exec(
			"DELETE FROM request_log_content WHERE tenant_id = ? AND length(input_content) = 0 AND length(output_content) = 0 AND length(detail_content) = 0 AND session_id = ''",
			options.TenantID,
		)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: delete empty request_log_content rows: %w", err)
		}
		result.DeletedContents, err = rowsAffected(deleteEmptyRowsResult)
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: deleted empty content rows: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return ClearRequestLogsResult{}, fmt.Errorf("usage: commit clear request logs: %w", err)
	}
	committed = true
	refreshRequestLogContentBytes(db)

	if usageDriver == "sqlite" {
		_, err := db.Exec("VACUUM")
		if err != nil {
			log.Warnf("usage: vacuum after request log cleanup failed: %v", err)
		}
	} else {
		// PostgreSQL maintenance is asynchronous and online; never make this
		// management request wait for DDL or database maintenance.
		triggerRequestLogCompaction()
	}

	if result.DeletedLogs > 0 || result.DeletedContents > 0 || result.ClearedBodyRows > 0 || result.ClearedDetailRows > 0 || result.ClearedLegacyRows > 0 {
		log.Infof(
			"usage: cleared request logs (logs=%d contents=%d body_rows=%d detail_rows=%d legacy_rows=%d)",
			result.DeletedLogs,
			result.DeletedContents,
			result.ClearedBodyRows,
			result.ClearedDetailRows,
			result.ClearedLegacyRows,
		)
	}

	return result, nil
}

// ClearAllRequestLogs removes all request_logs and request_log_content rows
// while leaving other SQLite-backed management data untouched.
func ClearAllRequestLogs() (ClearRequestLogsResult, error) {
	return ClearAllRequestLogsForTenant(systemTenantID)
}

func ClearAllRequestLogsForTenant(tenantID string) (ClearRequestLogsResult, error) {
	return ClearRequestLogs(ClearRequestLogsOptions{TenantID: tenantID, ClearRequestRecords: true})
}

// normalizeLogQueryParams merges deprecated single-value fields into their
// multi-value counterparts. It trims spaces, deduplicates, and removes empties.
func normalizeLogQueryParams(params LogQueryParams) LogQueryParams {
	params.TenantID = normalizeTenantID(params.TenantID)
	params.EndUserID = strings.TrimSpace(params.EndUserID)
	if params.APIKey != "" {
		params.APIKeys = append(params.APIKeys, params.APIKey)
		params.APIKey = ""
	}
	if params.Model != "" {
		params.Models = append(params.Models, params.Model)
		params.Model = ""
	}
	if params.Status != "" {
		params.Statuses = append(params.Statuses, params.Status)
		params.Status = ""
	}
	params.APIKeys = normalizeStringList(params.APIKeys)
	params.APIKeyIDs = normalizeStringList(params.APIKeyIDs)
	params.Models = normalizeStringList(params.Models)
	params.Statuses = normalizeStringList(params.Statuses)
	return params
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, v := range values {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if _, ok := seen[lower]; ok {
			continue
		}
		seen[lower] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func buildWhereClause(params LogQueryParams) (string, []interface{}) {
	params = normalizeLogQueryParams(params)
	if params.MatchNoAPIKeys || params.MatchNoAPIKeyIDs || params.MatchNoModels || params.MatchNoStatuses || params.MatchNoChannels {
		return " WHERE 1 = 0", nil
	}
	conditions := make([]string, 0, 5)
	args := make([]interface{}, 0, 5)
	conditions = append(conditions, "tenant_id = ?")
	args = append(args, params.TenantID)

	// Time range: days=1 means "today", days=7 means "last 7 days", etc.
	conditions = append(conditions, "timestamp >= ?")
	args = append(args, CutoffStartUTC(params.Days).Format(time.RFC3339))

	if params.EndUserID != "" {
		predicate, selectorArgs := buildEndUserAPIKeySelectorPredicate(params.TenantID, params.EndUserID)
		conditions = append(conditions, predicate)
		args = append(args, selectorArgs...)
	}

	// Stable key-id multi-value filter (public key facet; does not expand account pools).
	if len(params.APIKeyIDs) > 0 {
		idConds := make([]string, 0, len(params.APIKeyIDs))
		for _, id := range params.APIKeyIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			row := GetAPIKeyByIDForTenant(params.TenantID, id)
			if row == nil {
				row = GetAPIKeyByID(id)
			}
			if row != nil {
				secret := strings.TrimSpace(row.Key)
				if secret != "" {
					idConds = append(idConds, "(api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))")
					args = append(args, id, secret)
					continue
				}
			}
			idConds = append(idConds, "api_key_id = ?")
			args = append(args, id)
		}
		if len(idConds) == 1 {
			conditions = append(conditions, idConds[0])
		} else if len(idConds) > 1 {
			conditions = append(conditions, "("+strings.Join(idConds, " OR ")+")")
		}
	}

	// API Key multi-value filter
	if len(params.APIKeys) > 0 {
		apiKeyConds := make([]string, 0, len(params.APIKeys))
		systemConds := make([]string, 0)
		normalKeys := make([]string, 0, len(params.APIKeys))
		for _, key := range params.APIKeys {
			if key == systemRequestLogFilterValue {
				systemConds = append(systemConds, `(
					trim(coalesce(api_key_name, '')) = ''
					AND (
						trim(coalesce(api_key, '')) = ''
						OR trim(coalesce(api_key, '')) LIKE '/%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'GET /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'POST /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'PUT /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'PATCH /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'DELETE /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'OPTIONS /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'HEAD /%'
					)
				)`)
			} else {
				normalKeys = append(normalKeys, key)
			}
		}
		if len(normalKeys) > 0 {
			for _, k := range normalKeys {
				clause, clauseArgs := buildSingleAPIKeySelectorClauseForTenant(params.TenantID, k)
				apiKeyConds = append(apiKeyConds, strings.TrimPrefix(clause, " WHERE "))
				args = append(args, clauseArgs...)
			}
		}
		apiKeyConds = append(apiKeyConds, systemConds...)
		if len(apiKeyConds) == 1 {
			conditions = append(conditions, apiKeyConds[0])
		} else if len(apiKeyConds) > 1 {
			conditions = append(conditions, "("+strings.Join(apiKeyConds, " OR ")+")")
		}
	}

	// Model multi-value filter
	if len(params.Models) > 0 {
		placeholders := make([]string, 0, len(params.Models))
		for _, m := range params.Models {
			placeholders = append(placeholders, "?")
			args = append(args, m)
		}
		conditions = append(conditions, "model IN ("+strings.Join(placeholders, ",")+")")
	}

	// Status multi-value filter
	if len(params.Statuses) > 0 {
		hasSuccess := false
		hasFailed := false
		for _, s := range params.Statuses {
			switch strings.ToLower(s) {
			case "success":
				hasSuccess = true
			case "failed":
				hasFailed = true
			}
		}
		if hasSuccess && !hasFailed {
			conditions = append(conditions, "failed = 0")
		} else if hasFailed && !hasSuccess {
			conditions = append(conditions, "failed = 1")
		}
		// Both success and failed: no status filter needed (equivalent to "all")
	}
	if len(params.AuthSubjectIDs) > 0 || len(params.AuthIndexes) > 0 || len(params.ChannelNames) > 0 {
		filterConditions := make([]string, 0, 3)

		subjectPlaceholders := make([]string, 0, len(params.AuthSubjectIDs))
		for _, subjectID := range params.AuthSubjectIDs {
			trimmed := strings.TrimSpace(subjectID)
			if trimmed == "" {
				continue
			}
			subjectPlaceholders = append(subjectPlaceholders, "?")
			args = append(args, trimmed)
		}
		if len(subjectPlaceholders) > 0 {
			// Account-level filter: one subject covers all credential-instance indexes.
			filterConditions = append(filterConditions, "auth_subject_id IN ("+strings.Join(subjectPlaceholders, ",")+")")
		}

		authPlaceholders := make([]string, 0, len(params.AuthIndexes))
		for _, idx := range params.AuthIndexes {
			trimmed := strings.TrimSpace(idx)
			if trimmed == "" {
				continue
			}
			authPlaceholders = append(authPlaceholders, "?")
			args = append(args, trimmed)
		}
		if len(authPlaceholders) > 0 {
			// Match by auth_index regardless of channel_name so multi-provider
			// accounts that share a display email/label stay independently filterable.
			filterConditions = append(filterConditions, "auth_index IN ("+strings.Join(authPlaceholders, ",")+")")
		}

		for idx, names := range params.AuthIndexChannelNames {
			trimmedIndex := strings.TrimSpace(idx)
			if trimmedIndex == "" {
				continue
			}
			pairPlaceholders := make([]string, 0, len(names))
			pairArgs := make([]any, 0, len(names)+1)
			pairArgs = append(pairArgs, trimmedIndex)
			for _, name := range names {
				trimmedName := strings.ToLower(strings.TrimSpace(name))
				if trimmedName == "" {
					continue
				}
				pairPlaceholders = append(pairPlaceholders, "?")
				pairArgs = append(pairArgs, trimmedName)
			}
			if len(pairPlaceholders) == 0 {
				continue
			}
			filterConditions = append(filterConditions, "(auth_index = ? AND lower(trim(channel_name)) IN ("+strings.Join(pairPlaceholders, ",")+"))")
			args = append(args, pairArgs...)
		}

		channelPlaceholders := make([]string, 0, len(params.ChannelNames))
		for _, name := range params.ChannelNames {
			trimmed := strings.ToLower(strings.TrimSpace(name))
			if trimmed == "" {
				continue
			}
			channelPlaceholders = append(channelPlaceholders, "?")
			args = append(args, trimmed)
		}
		if len(channelPlaceholders) > 0 {
			filterConditions = append(filterConditions, "lower(trim(channel_name)) IN ("+strings.Join(channelPlaceholders, ",")+")")
		}

		if len(filterConditions) > 0 {
			conditions = append(conditions, "("+strings.Join(filterConditions, " OR ")+")")
		} else {
			// If caller attempted to filter but provided no usable channel selectors, match nothing.
			conditions = append(conditions, "1 = 0")
		}
	}

	if len(conditions) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func queryDistinct(db *sql.DB, column string, params LogQueryParams) ([]string, error) {
	where, args := buildWhereClause(params)
	q := fmt.Sprintf("SELECT DISTINCT %s FROM request_logs%s ORDER BY %s", column, where, column)
	rows, err := db.Query(q, args...)
	if err != nil {
		log.Warnf("usage: distinct %s query failed: %v", column, err)
		return nil, fmt.Errorf("usage: distinct %s: %w", column, err)
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			log.Warnf("usage: distinct %s scan failed: %v", column, err)
			return nil, err
		}
		if v != "" {
			result = append(result, v)
		}
	}
	return result, nil
}

// queryDistinctChannelRows returns account-level channel options.
// Rows with auth_subject_id collapse onto one option (provider-scoped account).
// Orphan rows without subject keep auth_index/channel_name legacy options.
func queryDistinctChannelRows(db *sql.DB, params LogQueryParams) ([]ChannelFilterOption, error) {
	where, args := buildWhereClause(params)
	// Aggregate channel_name/auth_index with MAX so PostgreSQL accepts subject-level
	// collapse (only subject is a real grouping key when subject is non-empty).
	q := `
		SELECT
			trim(coalesce(auth_subject_id, '')) AS auth_subject_id,
			MAX(trim(coalesce(channel_name, ''))) AS channel_name,
			MAX(trim(coalesce(auth_index, ''))) AS auth_index,
			MAX(trim(coalesce(source, ''))) AS source,
			MAX(trim(coalesce(model, ''))) AS model
		FROM request_logs` + where + `
		GROUP BY
			trim(coalesce(auth_subject_id, '')),
			CASE
				WHEN trim(coalesce(auth_subject_id, '')) <> '' THEN ''
				ELSE trim(coalesce(channel_name, ''))
			END,
			CASE
				WHEN trim(coalesce(auth_subject_id, '')) <> '' THEN ''
				ELSE trim(coalesce(auth_index, ''))
			END
		ORDER BY channel_name, auth_subject_id, auth_index`
	rows, err := db.Query(q, args...)
	if err != nil {
		log.Warnf("usage: distinct channel rows query failed: %v", err)
		return nil, fmt.Errorf("usage: distinct channel rows: %w", err)
	}
	defer rows.Close()

	result := make([]ChannelFilterOption, 0)
	seenSubject := make(map[string]struct{})
	for rows.Next() {
		var subjectID, channelName, authIndex, source, model string
		if err := rows.Scan(&subjectID, &channelName, &authIndex, &source, &model); err != nil {
			log.Warnf("usage: distinct channel rows scan failed: %v", err)
			return nil, err
		}
		subjectID = strings.TrimSpace(subjectID)
		channelName = strings.TrimSpace(channelName)
		authIndex = strings.TrimSpace(authIndex)
		source = strings.TrimSpace(source)
		model = strings.TrimSpace(model)
		if subjectID == "" && channelName == "" && authIndex == "" {
			continue
		}
		provider, authType := InferChannelDisplayMeta(channelName, source, model, "")
		if subjectID != "" {
			if _, ok := seenSubject[subjectID]; ok {
				continue
			}
			seenSubject[subjectID] = struct{}{}
			label := channelName
			if label == "" {
				label = subjectID
			}
			result = append(result, ChannelFilterOption{
				Value:         subjectID,
				Label:         label,
				AuthIndex:     authIndex,
				AuthSubjectID: subjectID,
				Provider:      provider,
				AuthType:      authType,
			})
			continue
		}
		label := channelName
		if label == "" {
			label = authIndex
		}
		value := authIndex
		if value == "" {
			value = channelName
		}
		result = append(result, ChannelFilterOption{
			Value:     value,
			Label:     label,
			AuthIndex: authIndex,
			Provider:  provider,
			AuthType:  authType,
		})
	}
	return result, rows.Err()
}

// InferChannelDisplayMeta derives provider + auth_type for channel filter chips
// when live auth metadata is missing (historical / deleted credentials).
func InferChannelDisplayMeta(label, source, model, providerHint string) (provider, authType string) {
	provider = normalizeChannelProvider(providerHint)
	label = strings.TrimSpace(label)
	source = strings.TrimSpace(source)
	model = strings.ToLower(strings.TrimSpace(model))

	apiKeySource := looksLikeAPIKeySource(source)
	if provider == "" && !apiKeySource {
		// Never treat raw API keys as provider ids.
		provider = normalizeChannelProvider(guessProviderFromSource(source))
	}
	if provider == "" {
		provider = normalizeChannelProvider(guessProviderFromLabel(label))
	}
	if provider == "" {
		provider = normalizeChannelProvider(guessProviderFromModel(model))
	}

	switch {
	case apiKeySource:
		authType = "api"
	case strings.Contains(label, "@") || strings.Contains(source, "@"):
		authType = "oauth"
	case provider == "opencode-go" || provider == "openai-compatibility":
		authType = "api"
	case provider != "":
		// Named upstream providers default to oauth-style channels when not an API key source.
		authType = "oauth"
	default:
		authType = ""
	}
	return provider, authType
}

func normalizeChannelProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "":
		return ""
	case "openai-compatibility", "openai_compat", "openai-compat":
		return "opencode-go"
	case "opencode", "opencode go", "opencode_go", "opencodego":
		return "opencode-go"
	case "grok":
		return "xai"
	case "gemini-cli", "geminicli":
		return "gemini"
	default:
		return value
	}
}

func guessProviderFromLabel(label string) string {
	key := strings.ToLower(strings.TrimSpace(label))
	switch {
	case key == "":
		return ""
	case key == "opencode go" || strings.HasPrefix(key, "opencode go") || strings.Contains(key, "opencode-go"):
		return "opencode-go"
	case strings.Contains(key, "codex"):
		return "codex"
	case strings.Contains(key, "claude") || strings.Contains(key, "anthropic"):
		return "claude"
	case strings.Contains(key, "gemini") || strings.Contains(key, "antigravity"):
		return "gemini"
	case strings.Contains(key, "xai") || strings.Contains(key, "grok"):
		return "xai"
	case strings.Contains(key, "kimi"):
		return "kimi"
	case strings.Contains(key, "iflow"):
		return "iflow"
	default:
		return ""
	}
}

func guessProviderFromModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case model == "":
		return ""
	case strings.HasPrefix(model, "grok") || strings.Contains(model, "grok-"):
		return "xai"
	case strings.HasPrefix(model, "gpt-") || strings.Contains(model, "codex") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4"):
		return "codex"
	case strings.HasPrefix(model, "claude"):
		return "claude"
	case strings.HasPrefix(model, "gemini") || strings.Contains(model, "antigravity"):
		return "gemini"
	case strings.HasPrefix(model, "kimi") || strings.Contains(model, "moonshot"):
		return "kimi"
	case strings.HasPrefix(model, "deepseek") || strings.HasPrefix(model, "glm") || strings.HasPrefix(model, "qwen") || strings.HasPrefix(model, "minimax") || strings.HasPrefix(model, "mimo"):
		// Common OpenCode Go / compat model ids when channel label is missing.
		return "opencode-go"
	default:
		return ""
	}
}

func looksLikeAPIKeySource(source string) bool {
	source = strings.TrimSpace(source)
	if source == "" {
		return false
	}
	lower := strings.ToLower(source)
	return strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "api-") ||
		strings.HasPrefix(lower, "key-") ||
		strings.HasPrefix(lower, "api_key:") ||
		(len(source) >= 24 && !strings.Contains(source, "@") && !strings.Contains(source, " "))
}

func guessProviderFromSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return ""
	}
	// Source may be an email or a provider id; only return known-looking short keys.
	if strings.Contains(source, "@") || strings.Contains(source, " ") {
		return ""
	}
	if len(source) > 32 {
		return ""
	}
	return source
}

func queryDistinctStatuses(db *sql.DB, params LogQueryParams) ([]string, error) {
	where, args := buildWhereClause(params)
	rows, err := db.Query("SELECT DISTINCT failed FROM request_logs"+where+" ORDER BY failed", args...)
	if err != nil {
		return nil, fmt.Errorf("usage: distinct statuses: %w", err)
	}
	defer rows.Close()

	result := make([]string, 0, 2)
	for rows.Next() {
		var failed int
		if err := rows.Scan(&failed); err != nil {
			return nil, err
		}
		if failed == 0 {
			result = append(result, "success")
		} else {
			result = append(result, "failed")
		}
	}
	return result, rows.Err()
}

func trimNullString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

// QueryModelsForKey returns the distinct models used by a specific API key within the time range.
func QueryModelsForKey(apiKey string, days int) ([]string, error) {
	db := getReadDB()
	if db == nil {
		return make([]string, 0), nil
	}
	if days < 1 {
		days = 7
	}
	params := LogQueryParams{TenantID: ResolveAPIKeyTenant(apiKey), APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)
	rows, err := db.Query(
		"SELECT DISTINCT model FROM request_logs"+where+" AND model != '' ORDER BY model",
		args...,
	)
	if err != nil {
		log.Warnf("usage: distinct models for key query failed: %v", err)
		return nil, fmt.Errorf("usage: distinct models for key: %w", err)
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			log.Warnf("usage: distinct models for key scan failed: %v", err)
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

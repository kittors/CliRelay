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

	db := getDB()
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
		return LogQueryResult{}, fmt.Errorf("usage: count query: %w", err)
	}

	// Fetch page
	offset := (params.Page - 1) * params.Size
	querySQL := "SELECT id, timestamp, api_key, api_key_name, model, source, channel_name, auth_index, " +
		"failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, " +
		"cost, " +
		"(CASE WHEN EXISTS (SELECT 1 FROM request_log_content content WHERE content.log_id = request_logs.id) " +
		"OR length(input_content) > 0 OR length(output_content) > 0 THEN 1 ELSE 0 END) as has_content " +
		"FROM request_logs" + where +
		" ORDER BY timestamp DESC LIMIT ? OFFSET ?"
	queryArgs := append(args, params.Size, offset)

	rows, err := db.Query(querySQL, queryArgs...)
	if err != nil {
		return LogQueryResult{}, fmt.Errorf("usage: query logs: %w", err)
	}
	defer rows.Close()

	items := make([]LogRow, 0, params.Size)
	for rows.Next() {
		var row LogRow
		var ts string
		var failedInt, hasContentInt int
		if err := rows.Scan(
			&row.ID, &ts, &row.APIKey, &row.APIKeyName, &row.Model, &row.Source, &row.ChannelName,
			&row.AuthIndex, &failedInt, &row.LatencyMs, &row.FirstTokenMs,
			&row.InputTokens, &row.OutputTokens, &row.ReasoningTokens,
			&row.CachedTokens, &row.TotalTokens, &row.Cost, &hasContentInt,
		); err != nil {
			return LogQueryResult{}, fmt.Errorf("usage: scan row: %w", err)
		}
		row.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		row.Failed = failedInt != 0
		row.HasContent = hasContentInt != 0
		items = append(items, row)
	}

	return LogQueryResult{
		Items: items,
		Total: total,
		Page:  params.Page,
		Size:  params.Size,
	}, nil
}

// QueryFilters returns the distinct API keys and models within the time range.
func QueryFilters(days int) (FilterOptions, error) {
	if days < 1 {
		days = 7
	}
	db := getDB()
	if db == nil {
		// Ensure stable JSON shape: slices => [] (not null), maps => {} (not null).
		return FilterOptions{
			APIKeys:     make([]string, 0),
			APIKeyNames: make(map[string]string),
			Models:      make([]string, 0),
			Channels:    make([]string, 0),
		}, nil
	}

	cutoff := CutoffStartUTC(days).Format(time.RFC3339)

	keys, keyNames, err := queryDistinctAPIKeys(db, cutoff)
	if err != nil {
		return FilterOptions{}, err
	}
	models, err := queryDistinct(db, "model", cutoff)
	if err != nil {
		return FilterOptions{}, err
	}
	channels, err := queryDistinct(db, "channel_name", cutoff)
	if err != nil {
		return FilterOptions{}, err
	}

	return FilterOptions{
		APIKeys:     keys,
		APIKeyNames: keyNames,
		Models:      models,
		Channels:    channels,
	}, nil
}

// QueryStats returns aggregated statistics over the filtered dataset.
func QueryStats(params LogQueryParams) (LogStats, error) {
	db := getDB()
	if db == nil {

		return LogStats{CacheRate: 0}, nil
	}
	if params.Days < 1 {
		params.Days = 7
	}

	where, args := buildWhereClause(params)

	var total, successCount, totalTokens, effectiveInputTokens, totalCachedTokens int64
	var totalCost float64
	statsSQL := "SELECT COUNT(*), COALESCE(SUM(CASE WHEN failed=0 THEN 1 ELSE 0 END),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0), COALESCE(SUM(" + cacheRateEffectiveInputSQL + "),0), COALESCE(SUM(cached_tokens),0) " +
		"FROM request_logs" + where
	if err := db.QueryRow(statsSQL, args...).Scan(&total, &successCount, &totalTokens, &totalCost, &effectiveInputTokens, &totalCachedTokens); err != nil {
		return LogStats{}, fmt.Errorf("usage: stats query: %w", err)
	}

	var successRate float64
	if total > 0 {
		successRate = float64(successCount) / float64(total) * 100
	}

	return LogStats{
		Total:       total,
		SuccessRate: successRate,
		TotalTokens: totalTokens,
		CacheRate:   cacheRateFromTokenTotals(effectiveInputTokens, totalCachedTokens),
		TotalCost:   totalCost,
	}, nil
}

// DeleteLogsByAPIKey removes all request_logs and request_log_content entries
// for the given API key. Returns the number of deleted log rows.
func DeleteLogsByAPIKey(apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, fmt.Errorf("usage: database not initialised")
	}
	if apiKey == "" {
		return 0, fmt.Errorf("usage: empty api_key")
	}

	// Delete associated content rows first (FK cascade may handle this,
	// but be explicit to ensure cleanup even without FK enforcement).
	clause, args := buildSingleAPIKeySelectorClause(apiKey)
	_, _ = db.Exec(
		`DELETE FROM request_log_content WHERE log_id IN
		 (SELECT id FROM request_logs`+clause+`)`, args...)

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
		contentResult, err := tx.Exec("DELETE FROM request_log_content")
		if err != nil {
			return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content: %w", err)
		}
		logResult, err := tx.Exec("DELETE FROM request_logs")
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
			contentResult, err := tx.Exec(
				"UPDATE request_log_content SET input_content = X'', output_content = X'' WHERE length(input_content) > 0 OR length(output_content) > 0",
			)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: clear request_log_content bodies: %w", err)
			}
			result.ClearedBodyRows, err = rowsAffected(contentResult)
			if err != nil {
				return ClearRequestLogsResult{}, fmt.Errorf("usage: body affected rows: %w", err)
			}

			legacyResult, err := tx.Exec(
				"UPDATE request_logs SET input_content = '', output_content = '' WHERE length(input_content) > 0 OR length(output_content) > 0",
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
				"UPDATE request_log_content SET detail_content = X'' WHERE length(detail_content) > 0",
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
			"DELETE FROM request_log_content WHERE length(input_content) = 0 AND length(output_content) = 0 AND length(detail_content) = 0",
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

	if _, err := db.Exec("VACUUM"); err != nil {
		log.Warnf("usage: vacuum after request log cleanup failed: %v", err)
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
	return ClearRequestLogs(ClearRequestLogsOptions{ClearRequestRecords: true})
}

// normalizeLogQueryParams merges deprecated single-value fields into their
// multi-value counterparts. It trims spaces, deduplicates, and removes empties.
func normalizeLogQueryParams(params LogQueryParams) LogQueryParams {
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
	conditions := make([]string, 0, 4)
	args := make([]interface{}, 0, 4)

	// Time range: days=1 means "today", days=7 means "last 7 days", etc.
	conditions = append(conditions, "timestamp >= ?")
	args = append(args, CutoffStartUTC(params.Days).Format(time.RFC3339))

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
				clause, clauseArgs := buildSingleAPIKeySelectorClause(k)
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
	if len(params.AuthIndexes) > 0 || len(params.ChannelNames) > 0 {
		filterConditions := make([]string, 0, 2)

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
			filterConditions = append(filterConditions, "(auth_index IN ("+strings.Join(authPlaceholders, ",")+") AND trim(coalesce(channel_name, '')) = '')")
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

func queryDistinct(db *sql.DB, column, cutoff string) ([]string, error) {
	q := fmt.Sprintf("SELECT DISTINCT %s FROM request_logs WHERE timestamp >= ? ORDER BY %s", column, column)
	rows, err := db.Query(q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("usage: distinct %s: %w", column, err)
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if v != "" {
			result = append(result, v)
		}
	}
	return result, nil
}

func queryDistinctAPIKeys(db *sql.DB, cutoff string) ([]string, map[string]string, error) {
	currentByID := currentAPIKeyRowsByID()
	rows, err := db.Query(`
		SELECT
			CASE
				WHEN trim(coalesce(api_key_id, '')) <> '' THEN api_key_id
				ELSE 'raw:' || api_key
			END AS logical_selector,
			COALESCE(MAX(NULLIF(trim(coalesce(api_key_id, '')), '')), '') AS logical_id,
			MAX(api_key) AS snapshot_key,
			COALESCE(NULLIF(MAX(api_key_name), ''), '') AS snapshot_name
		FROM request_logs
		WHERE timestamp >= ? AND api_key != ''
		GROUP BY logical_selector
		ORDER BY logical_selector
	`, cutoff)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: distinct api_key logical groups: %w", err)
	}
	defer rows.Close()

	values := make([]string, 0)
	names := make(map[string]string)
	seen := make(map[string]struct{})
	for rows.Next() {
		var logicalSelector string
		var logicalID sql.NullString
		var snapshotKey string
		var snapshotName string
		if err := rows.Scan(&logicalSelector, &logicalID, &snapshotKey, &snapshotName); err != nil {
			return nil, nil, err
		}

		value := strings.TrimSpace(snapshotKey)
		name := strings.TrimSpace(snapshotName)
		if row, ok := currentByID[trimNullString(logicalID)]; ok {
			if trimmed := strings.TrimSpace(row.Key); trimmed != "" {
				value = trimmed
			}
			if trimmed := strings.TrimSpace(row.Name); trimmed != "" {
				name = trimmed
			}
		}
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			if name != "" {
				names[value] = name
			}
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
		if name != "" {
			names[value] = name
		}
	}
	return values, names, rows.Err()
}

func trimNullString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return strings.TrimSpace(value.String)
}

func buildSingleAPIKeySelectorClause(selector string) (string, []interface{}) {
	trimmed := strings.TrimSpace(selector)
	if trimmed == "" {
		return "", nil
	}
	if identity := ResolveAPIKeyIdentity(trimmed); identity != nil {
		return " WHERE (api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))", []interface{}{identity.ID, identity.Key}
	}
	return " WHERE api_key = ?", []interface{}{trimmed}
}

// QueryModelsForKey returns the distinct models used by a specific API key within the time range.
func QueryModelsForKey(apiKey string, days int) ([]string, error) {
	db := getDB()
	if db == nil {
		return make([]string, 0), nil
	}
	if days < 1 {
		days = 7
	}
	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)
	rows, err := db.Query(
		"SELECT DISTINCT model FROM request_logs"+where+" AND model != '' ORDER BY model",
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("usage: distinct models for key: %w", err)
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

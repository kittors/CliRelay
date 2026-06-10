package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DailySeriesPoint holds one day of aggregated usage data.
type DailySeriesPoint struct {
	Date         string `json:"date"`
	Requests     int    `json:"requests"`
	FailedReq    int    `json:"failed_requests"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// ModelDistributionPoint holds aggregated usage data for a single model.
type ModelDistributionPoint struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// QueryDailySeries returns per-day aggregated request count and token usage for a given API key.
func QueryDailySeries(apiKey string, days int) ([]DailySeriesPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	// NOTE: timestamps are stored as UTC RFC3339 strings; localtime converts them to the process timezone
	// (configured via TZ/time.Local) for correct day bucketing.
	q := `SELECT date(timestamp, 'localtime') as d,
	             COUNT(*) as reqs,
	             SUM(CASE WHEN failed = 1 OR failed = 'true' THEN 1 ELSE 0 END) as failed_reqs,
	             COALESCE(SUM(input_tokens),0),
	             COALESCE(SUM(output_tokens),0)
	      FROM request_logs` + where + `
	      GROUP BY d ORDER BY d`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily series query: %w", err)
	}
	defer rows.Close()

	var result []DailySeriesPoint
	for rows.Next() {
		var p DailySeriesPoint
		if err := rows.Scan(&p.Date, &p.Requests, &p.FailedReq, &p.InputTokens, &p.OutputTokens); err != nil {
			return nil, fmt.Errorf("usage: daily series scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// QueryModelDistribution returns request count and token usage grouped by model for a given API key.
func QueryModelDistribution(apiKey string, days int) ([]ModelDistributionPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)

	q := `SELECT model,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0)
	      FROM request_logs` + where + `
	      GROUP BY model ORDER BY reqs DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: model distribution query: %w", err)
	}
	defer rows.Close()

	var result []ModelDistributionPoint
	for rows.Next() {
		var p ModelDistributionPoint
		if err := rows.Scan(&p.Model, &p.Requests, &p.Tokens); err != nil {
			return nil, fmt.Errorf("usage: model distribution scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// APIKeyDistributionPoint holds aggregated usage data for a single API key.
type APIKeyDistributionPoint struct {
	APIKey   string `json:"api_key"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// QueryAPIKeyDistribution returns request count and token usage grouped by api_key.
func QueryAPIKeyDistribution(days int) ([]APIKeyDistributionPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}

	params := LogQueryParams{Days: days}
	where, args := buildWhereClause(params)
	currentByID := currentAPIKeyRowsByID()

	q := `SELECT
	             CASE
	               WHEN trim(coalesce(api_key_id, '')) <> '' THEN api_key_id
	               ELSE 'raw:' || api_key
	             END as logical_selector,
	             COALESCE(MAX(NULLIF(trim(coalesce(api_key_id, '')), '')), '') as logical_id,
	             MAX(api_key) as snapshot_key,
	             COALESCE(NULLIF(MAX(api_key_name),''), '') as snapshot_name,
	             COUNT(*) as reqs,
	             COALESCE(SUM(total_tokens),0)
	      FROM request_logs` + where + `
	      AND api_key != ''
	      GROUP BY logical_selector ORDER BY reqs DESC`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: apikey distribution query: %w", err)
	}
	defer rows.Close()

	var result []APIKeyDistributionPoint
	for rows.Next() {
		var logicalSelector string
		var logicalID sql.NullString
		var snapshotKey string
		var snapshotName string
		var p APIKeyDistributionPoint
		if err := rows.Scan(&logicalSelector, &logicalID, &snapshotKey, &snapshotName, &p.Requests, &p.Tokens); err != nil {
			return nil, fmt.Errorf("usage: apikey distribution scan: %w", err)
		}
		p.APIKey = strings.TrimSpace(snapshotKey)
		p.Name = strings.TrimSpace(snapshotName)
		if row, ok := currentByID[trimNullString(logicalID)]; ok {
			if trimmed := strings.TrimSpace(row.Key); trimmed != "" {
				p.APIKey = trimmed
			}
			if trimmed := strings.TrimSpace(row.Name); trimmed != "" {
				p.Name = trimmed
			}
		}
		if p.APIKey == "" {
			continue
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

// HourlyTokenPoint holds token usage per hour for the last N hours.
type HourlyTokenPoint struct {
	Hour            string `json:"hour"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
}

// HourlyModelPoint holds model request counts per hour.
type HourlyModelPoint struct {
	Hour     string `json:"hour"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// QueryHourlySeries returns per-hour token and model aggregates for the last N hours.
func QueryHourlySeries(apiKey string, hours int) ([]HourlyTokenPoint, []HourlyModelPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil, nil
	}
	if hours < 1 {
		hours = 24
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).UTC().Format(time.RFC3339)

	// Build WHERE clause directly with the correct hourly cutoff.
	// Previously this used buildWhereClause + strings.Replace, but that failed
	// because buildWhereClause uses parameterised queries (? placeholders)
	// so the time value lives in args, not in the where string.
	conditions := []string{"timestamp >= ?"}
	args := []interface{}{cutoff}
	if apiKey != "" {
		if identity := ResolveAPIKeyIdentity(apiKey); identity != nil {
			conditions = append(conditions, "(api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))")
			args = append(args, identity.ID, identity.Key)
		} else {
			conditions = append(conditions, "api_key = ?")
			args = append(args, apiKey)
		}
	}
	where := " WHERE " + strings.Join(conditions, " AND ")

	// query tokens by hour
	tokenQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h,
	                      COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
	                      COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(total_tokens),0)
	               FROM request_logs` + where + ` GROUP BY h ORDER BY h`
	tokenRows, err := db.Query(tokenQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly token query: %w", err)
	}
	defer tokenRows.Close()

	var tokens []HourlyTokenPoint
	for tokenRows.Next() {
		var p HourlyTokenPoint
		if err := tokenRows.Scan(&p.Hour, &p.InputTokens, &p.OutputTokens, &p.ReasoningTokens, &p.CachedTokens, &p.TotalTokens); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly token scan: %w", err)
		}
		tokens = append(tokens, p)
	}

	// query models by hour
	modelQuery := `SELECT strftime('%Y-%m-%d %H:00', timestamp, 'localtime') as h, model, COUNT(*) as reqs
	               FROM request_logs` + where + ` AND model != '' GROUP BY h, model ORDER BY h`
	modelRows, err := db.Query(modelQuery, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: hourly model query: %w", err)
	}
	defer modelRows.Close()

	var models []HourlyModelPoint
	for modelRows.Next() {
		var p HourlyModelPoint
		if err := modelRows.Scan(&p.Hour, &p.Model, &p.Requests); err != nil {
			return nil, nil, fmt.Errorf("usage: hourly model scan: %w", err)
		}
		models = append(models, p)
	}

	return tokens, models, nil
}

// EntityStatPoint holds aggregated usage data for a single entity (source or auth_index).
type EntityStatPoint struct {
	EntityName  string  `json:"entity_name"`
	Requests    int64   `json:"requests"`
	Failed      int64   `json:"failed"`
	AvgLatency  float64 `json:"avg_latency"`
	TotalTokens int64   `json:"total_tokens"`
}

// QueryEntityStats returns aggregates grouped by a given column (e.g. "source" or "auth_index").
// Time range is derived from days logic.
func QueryEntityStats(apiKey string, days int, groupColumn string, entityNames []string) ([]EntityStatPoint, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if days < 1 {
		days = 7
	}
	if groupColumn != "source" && groupColumn != "auth_index" {
		return nil, fmt.Errorf("usage: invalid group column")
	}

	params := LogQueryParams{APIKey: apiKey, Days: days}
	where, args := buildWhereClause(params)
	entityNames = normalizeEntityStatFilters(entityNames)
	if len(entityNames) > 0 {
		placeholders := make([]string, 0, len(entityNames))
		for _, name := range entityNames {
			placeholders = append(placeholders, "?")
			args = append(args, name)
		}
		if where == "" {
			where = " WHERE " + groupColumn + " IN (" + strings.Join(placeholders, ",") + ")"
		} else {
			where += " AND " + groupColumn + " IN (" + strings.Join(placeholders, ",") + ")"
		}
	}

	q := fmt.Sprintf(`
		SELECT %s, COUNT(*), COALESCE(SUM(failed),0), COALESCE(AVG(latency_ms),0), COALESCE(SUM(total_tokens),0)
		FROM request_logs%s AND %s != ''
		GROUP BY %s ORDER BY COUNT(*) DESC
	`, groupColumn, where, groupColumn, groupColumn)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: entity stats query: %w", err)
	}
	defer rows.Close()

	var result []EntityStatPoint
	for rows.Next() {
		var p EntityStatPoint
		if err := rows.Scan(&p.EntityName, &p.Requests, &p.Failed, &p.AvgLatency, &p.TotalTokens); err != nil {
			return nil, fmt.Errorf("usage: entity stats scan: %w", err)
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func normalizeEntityStatFilters(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

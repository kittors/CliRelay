package usage

import (
	"database/sql"
	"fmt"
	"time"
)

// GetRequestLogStorageBytes returns the approximate bytes currently occupied by
// stored request/response bodies. It includes compressed rows in
// request_log_content and any legacy inline content not yet migrated out of
// request_logs.
func GetRequestLogStorageBytes() (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}

	var totalBytes sql.NullInt64
	err := db.QueryRow(`
		SELECT
			COALESCE((
				SELECT SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER) + CAST(length(detail_content) AS INTEGER))
				FROM request_log_content
			), 0) +
			COALESCE((
				SELECT SUM(CAST(length(input_content) AS INTEGER) + CAST(length(output_content) AS INTEGER))
				FROM request_logs
				WHERE length(input_content) > 0 OR length(output_content) > 0
			), 0)
	`).Scan(&totalBytes)
	if err != nil {
		return 0, fmt.Errorf("usage: query request log storage bytes: %w", err)
	}
	if !totalBytes.Valid {
		return 0, nil
	}
	return totalBytes.Int64, nil
}

// ChannelLatency holds the average latency stats for a single channel (source).
type ChannelLatency struct {
	Source string  `json:"source"`
	Count  int64   `json:"count"`
	AvgMs  float64 `json:"avg_ms"`
}

// GetChannelAvgLatency returns average request latency grouped by source (channel)
// for the last N days.
func GetChannelAvgLatency(days int) ([]ChannelLatency, error) {
	db := getReadDB()
	if db == nil {
		return nil, fmt.Errorf("usage: database not initialised")
	}

	cutoff := CutoffStartUTC(days)
	rows, err := db.Query(`
		SELECT source, COUNT(*) as cnt, AVG(latency_ms) as avg_lat
		FROM request_logs
		WHERE timestamp > ? AND source != ''
		GROUP BY source
		ORDER BY avg_lat DESC
		LIMIT 5
	`, cutoff.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("usage: query channel latency: %w", err)
	}
	defer rows.Close()

	var result []ChannelLatency
	for rows.Next() {
		var cl ChannelLatency
		if err := rows.Scan(&cl.Source, &cl.Count, &cl.AvgMs); err != nil {
			return nil, fmt.Errorf("usage: scan channel latency: %w", err)
		}
		result = append(result, cl)
	}
	return result, rows.Err()
}

// CountTodayByKey returns the number of requests made by the given API key today (project timezone).
func CountTodayByKey(apiKey string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	clause, args := buildSingleAPIKeySelectorClause(apiKey)
	var count int64
	queryArgs := append(args, CutoffStartUTC(1).Format(time.RFC3339))
	err := db.QueryRow(
		"SELECT COUNT(*) FROM request_logs"+clause+" AND timestamp >= ?",
		queryArgs...,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count today: %w", err)
	}
	return count, nil
}

// CountTotalByKey returns the total number of requests made by the given API key.
func CountTotalByKey(apiKey string) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	clause, args := buildSingleAPIKeySelectorClause(apiKey)
	var count int64
	err := db.QueryRow("SELECT COUNT(*) FROM request_logs"+clause, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: count total: %w", err)
	}
	return count, nil
}

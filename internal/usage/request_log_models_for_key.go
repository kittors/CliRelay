package usage

import (
	"database/sql"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

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

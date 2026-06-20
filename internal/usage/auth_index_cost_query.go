package usage

import (
	"fmt"
	"strings"
	"time"
)

func QueryCostByAuthIndexSince(authIndex string, since time.Time) (float64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return 0, nil
	}
	var total float64
	err := db.QueryRow(`
		SELECT COALESCE(SUM(cost), 0)
		FROM request_logs
		WHERE timestamp >= ? AND auth_index = ?
	`, since.UTC().Format(time.RFC3339), authIndex).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: request cost by auth index query: %w", err)
	}
	return total, nil
}

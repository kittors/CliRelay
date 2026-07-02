package usage

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type DailyCountPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
}

type DailyUsagePoint struct {
	Date     string  `json:"date"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

type DailyQuotaPoint struct {
	Date    string   `json:"date"`
	Percent *float64 `json:"percent"`
	Samples int64    `json:"samples"`
}

type HourlyCountPoint struct {
	Hour     string `json:"hour"`
	Requests int64  `json:"requests"`
}

type HourlyUsagePoint struct {
	Hour     string  `json:"hour"`
	Requests int64   `json:"requests"`
	Cost     float64 `json:"cost"`
}

func QueryDailyCallsByAuthIndexes(authIndexes []string, days int) ([]DailyCountPoint, error) {
	db := getReadDB()
	if db == nil {
		return []DailyCountPoint{}, nil
	}
	if days < 1 {
		days = 7
	}
	if len(authIndexes) == 0 {
		return []DailyCountPoint{}, nil
	}

	seen := make(map[string]struct{}, len(authIndexes))
	normalized := make([]string, 0, len(authIndexes))
	for _, idx := range authIndexes {
		idx = strings.TrimSpace(idx)
		if idx == "" {
			continue
		}
		if _, ok := seen[idx]; ok {
			continue
		}
		seen[idx] = struct{}{}
		normalized = append(normalized, idx)
	}
	if len(normalized) == 0 {
		return []DailyCountPoint{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]interface{}, 0, len(normalized)+1)
	args = append(args, CutoffStartUTC(days).Format(time.RFC3339))
	for _, idx := range normalized {
		args = append(args, idx)
	}

	q := fmt.Sprintf(`
		SELECT timestamp
		FROM request_logs
		WHERE timestamp >= ? AND auth_index IN (%s)
		ORDER BY timestamp ASC
	`, placeholders)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: daily calls by auth indexes query: %w", err)
	}
	defer rows.Close()

	byDate := make(map[string]int64, days)
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("usage: daily calls by auth indexes scan: %w", err)
		}
		parsed, ok := parseStoredTime(ts)
		if !ok {
			continue
		}
		byDate[localDayKeyAt(parsed)]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]DailyCountPoint, 0, len(byDate))
	for date, requests := range byDate {
		point := DailyCountPoint{Date: date, Requests: requests}
		result = append(result, point)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})
	return result, nil
}

func QueryHourlyCallsByAuthIndex(authIndex string, hours int) ([]HourlyCountPoint, error) {
	db := getReadDB()
	if db == nil {
		return []HourlyCountPoint{}, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return []HourlyCountPoint{}, nil
	}
	if hours < 1 {
		hours = 5
	}
	if hours > 24 {
		hours = 24
	}

	loc := getUsageLocation()
	now := time.Now().In(loc).Truncate(time.Hour)
	start := now.Add(-time.Duration(hours-1) * time.Hour)
	buckets := make([]HourlyCountPoint, 0, hours)
	byKey := make(map[string]*HourlyCountPoint, hours)
	for i := 0; i < hours; i++ {
		key := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:00")
		buckets = append(buckets, HourlyCountPoint{Hour: key, Requests: 0})
		byKey[key] = &buckets[len(buckets)-1]
	}

	rows, err := db.Query(`
		SELECT timestamp
		FROM request_logs
		WHERE timestamp >= ? AND auth_index = ?
	`, start.UTC().Format(time.RFC3339), authIndex)
	if err != nil {
		return nil, fmt.Errorf("usage: hourly calls by auth index query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, fmt.Errorf("usage: hourly calls by auth index scan: %w", err)
		}
		parsed, ok := parseStoredTime(ts)
		if !ok {
			continue
		}
		key := parsed.In(loc).Truncate(time.Hour).Format("2006-01-02 15:00")
		if bucket := byKey[key]; bucket != nil {
			bucket.Requests++
		}
	}
	return buckets, rows.Err()
}

func QueryRequestCountByAuthIndexSince(authIndex string, since time.Time) (int64, error) {
	db := getReadDB()
	if db == nil {
		return 0, nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return 0, nil
	}
	var count int64
	err := db.QueryRow(`
		SELECT COUNT(*)
		FROM request_logs
		WHERE timestamp >= ? AND auth_index = ?
	`, since.UTC().Format(time.RFC3339), authIndex).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("usage: request count by auth index query: %w", err)
	}
	return count, nil
}

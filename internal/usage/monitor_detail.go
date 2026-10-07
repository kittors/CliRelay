package usage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Latency percentiles need per-request rows, so they read request_logs and are
// limited to its retention (7 days by default). Rather than pulling every
// latency into Go, SQL counts requests into fixed, roughly logarithmic bins and
// also reports each bin's min and max; percentiles then interpolate inside the
// observed [min, max] of the bin that holds the rank. A bin with one request is
// therefore exact, so small samples are exact and large ones stay within one
// bin's spread. The same SQL runs on SQLite and PostgreSQL (SQLite has no
// percentile function).

// monitorLatencyBoundsMs are exclusive upper bounds. Coarser display bands
// (1s, 2s, 5s, 10s, 20s, 30s, 60s, 120s) are all present so the panel can
// merge bins without splitting one.
var monitorLatencyBoundsMs = []int64{
	100, 150, 200, 300, 400, 500, 700,
	1000, 1500, 2000, 3000, 4000, 5000, 7000,
	10000, 15000, 20000, 25000, 30000, 40000, 50000,
	60000, 90000, 120000, 180000, 300000, 600000,
}

const monitorRecentFailureLimit = 12

func monitorLatencyBinCase(column string) string {
	var b strings.Builder
	b.WriteString("CASE")
	for i, bound := range monitorLatencyBoundsMs {
		// Bounds are package constants, not input, so they are inlined.
		fmt.Fprintf(&b, " WHEN %s < %d THEN %d", column, bound, i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(monitorLatencyBoundsMs))
	return b.String()
}

type monitorLatencyBin struct {
	count, sum, min, max int64
}

// monitorLatencyHistogram collects one measurement of successful requests in
// [from, to). column is "latency_ms" or "first_token_ms".
func monitorLatencyHistogram(filter MonitorFilter, from, to time.Time, column string) (MonitorLatencyStats, time.Time, error) {
	stats := MonitorLatencyStats{Histogram: make([]int64, len(monitorLatencyBoundsMs)+1)}
	db := getReadDB()
	if db == nil {
		return stats, time.Time{}, nil
	}
	query := `SELECT ` + monitorLatencyBinCase(column) + `, COUNT(*),
			COALESCE(SUM(` + column + `), 0), COALESCE(MIN(` + column + `), 0), COALESCE(MAX(` + column + `), 0),
			MIN(timestamp)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND failed = 0 AND ` + column + ` > 0`
	args := []any{filter.TenantID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	predicate, predicateArgs := filter.detailPredicate()
	query += predicate + ` GROUP BY 1`
	args = append(args, predicateArgs...)

	rows, err := db.Query(query, args...)
	if err != nil {
		return stats, time.Time{}, fmt.Errorf("usage: monitor latency histogram: %w", err)
	}
	defer rows.Close()
	bins := make([]monitorLatencyBin, len(stats.Histogram))
	var oldest time.Time
	var sum int64
	for rows.Next() {
		var index int
		var bin monitorLatencyBin
		var first storedTime
		if err := rows.Scan(&index, &bin.count, &bin.sum, &bin.min, &bin.max, &first); err != nil {
			return stats, time.Time{}, fmt.Errorf("usage: monitor latency scan: %w", err)
		}
		if index < 0 || index >= len(bins) {
			continue
		}
		bins[index] = bin
		stats.Histogram[index] = bin.count
		stats.Samples += bin.count
		sum += bin.sum
		stats.MaxMs = max(stats.MaxMs, bin.max)
		if first.Valid && (oldest.IsZero() || first.Time.Before(oldest)) {
			oldest = first.Time
		}
	}
	if err := rows.Err(); err != nil {
		return stats, time.Time{}, err
	}
	if stats.Samples > 0 {
		stats.AvgMs = float64(sum) / float64(stats.Samples)
		stats.P50Ms = monitorBinQuantile(bins, stats.Samples, 0.50)
		stats.P90Ms = monitorBinQuantile(bins, stats.Samples, 0.90)
		stats.P95Ms = monitorBinQuantile(bins, stats.Samples, 0.95)
		stats.P99Ms = monitorBinQuantile(bins, stats.Samples, 0.99)
	}
	return stats, oldest, nil
}

// monitorBinQuantile estimates the q-quantile with linear interpolation between
// closest ranks (the numpy default), assuming values spread evenly across each
// bin's observed [min, max].
func monitorBinQuantile(bins []monitorLatencyBin, total int64, q float64) float64 {
	if total <= 0 {
		return 0
	}
	pos := q * float64(total-1)
	lower := int64(math.Floor(pos))
	upper := int64(math.Ceil(pos))
	low := monitorBinValueAt(bins, lower)
	if upper == lower {
		return low
	}
	return low + (pos-float64(lower))*(monitorBinValueAt(bins, upper)-low)
}

// monitorBinValueAt estimates the rank-th smallest value (0-based).
func monitorBinValueAt(bins []monitorLatencyBin, rank int64) float64 {
	var seen int64
	for _, bin := range bins {
		if bin.count == 0 {
			continue
		}
		if rank < seen+bin.count {
			if bin.count == 1 {
				return float64(bin.min)
			}
			offset := float64(rank-seen) / float64(bin.count-1)
			return float64(bin.min) + offset*float64(bin.max-bin.min)
		}
		seen += bin.count
	}
	for i := len(bins) - 1; i >= 0; i-- {
		if bins[i].count > 0 {
			return float64(bins[i].max)
		}
	}
	return 0
}

// monitorRecentFailures lists the newest failed requests of the window.
func monitorRecentFailures(filter MonitorFilter, from, to time.Time, consumerLabel func(string) string) ([]MonitorFailure, error) {
	out := []MonitorFailure{}
	db := getReadDB()
	if db == nil {
		return out, nil
	}
	query := `SELECT id, timestamp, model, channel_name, api_key_id, latency_ms, streaming
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ? AND failed = 1`
	args := []any{filter.TenantID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}
	predicate, predicateArgs := filter.detailPredicate()
	query += predicate + ` ORDER BY timestamp DESC, id DESC LIMIT ` + strconv.Itoa(monitorRecentFailureLimit)
	args = append(args, predicateArgs...)

	rows, err := db.Query(query, args...)
	if err != nil {
		return out, fmt.Errorf("usage: monitor recent failures: %w", err)
	}
	defer rows.Close()
	ownerByKeyID := map[string]string{}
	for _, row := range ListAPIKeysForTenant(filter.TenantID) {
		if id := strings.TrimSpace(row.ID); id != "" {
			ownerByKeyID[id] = strings.TrimSpace(row.EndUserID)
		}
	}
	for rows.Next() {
		var item MonitorFailure
		var ts storedTime
		var apiKeyID string
		var streaming int
		if err := rows.Scan(&item.ID, &ts, &item.Model, &item.Channel, &apiKeyID, &item.LatencyMs, &streaming); err != nil {
			return out, fmt.Errorf("usage: monitor recent failures scan: %w", err)
		}
		if ts.Valid {
			item.Timestamp = ts.Time.UTC().Format(time.RFC3339)
		}
		item.Streaming = streaming != 0
		if key := monitorConsumerKey(ownerByKeyID[strings.TrimSpace(apiKeyID)], apiKeyID); key != "" {
			item.Consumer = consumerLabel(key)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

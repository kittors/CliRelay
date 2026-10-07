package usage

import (
	"fmt"
	"strings"
	"time"
)

// monitorAgg mirrors the additive columns of usage_rollup_buckets.
type monitorAgg struct {
	Requests        int64
	Success         int64
	Failed          int64
	Streaming       int64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CachedTokens    int64
	EffectiveInput  int64
	TotalTokens     int64
	Cost            float64
	LatencySum      int64
	LatencyCount    int64
	FirstTokenSum   int64
	FirstTokenCount int64
}

// monitorAggColumns must stay in the order of monitorAgg.scanTargets.
const monitorAggColumns = `
	COALESCE(SUM(request_count), 0),
	COALESCE(SUM(success_count), 0),
	COALESCE(SUM(failure_count), 0),
	COALESCE(SUM(streaming_count), 0),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(reasoning_tokens), 0),
	COALESCE(SUM(cached_tokens), 0),
	COALESCE(SUM(effective_input_tokens), 0),
	COALESCE(SUM(total_tokens), 0),
	COALESCE(SUM(cost_total), 0),
	COALESCE(SUM(latency_sum_ms), 0),
	COALESCE(SUM(latency_count), 0),
	COALESCE(SUM(first_token_sum_ms), 0),
	COALESCE(SUM(first_token_count), 0)`

func (a *monitorAgg) scanTargets() []any {
	return []any{
		&a.Requests, &a.Success, &a.Failed, &a.Streaming,
		&a.InputTokens, &a.OutputTokens, &a.ReasoningTokens, &a.CachedTokens,
		&a.EffectiveInput, &a.TotalTokens, &a.Cost,
		&a.LatencySum, &a.LatencyCount, &a.FirstTokenSum, &a.FirstTokenCount,
	}
}

func (a *monitorAgg) add(o monitorAgg) {
	a.Requests += o.Requests
	a.Success += o.Success
	a.Failed += o.Failed
	a.Streaming += o.Streaming
	a.InputTokens += o.InputTokens
	a.OutputTokens += o.OutputTokens
	a.ReasoningTokens += o.ReasoningTokens
	a.CachedTokens += o.CachedTokens
	a.EffectiveInput += o.EffectiveInput
	a.TotalTokens += o.TotalTokens
	a.Cost += o.Cost
	a.LatencySum += o.LatencySum
	a.LatencyCount += o.LatencyCount
	a.FirstTokenSum += o.FirstTokenSum
	a.FirstTokenCount += o.FirstTokenCount
}

func (a monitorAgg) totals() MonitorTotals {
	t := MonitorTotals{
		Requests:        a.Requests,
		Success:         a.Success,
		Failed:          a.Failed,
		Streaming:       a.Streaming,
		InputTokens:     a.InputTokens,
		OutputTokens:    a.OutputTokens,
		ReasoningTokens: a.ReasoningTokens,
		CachedTokens:    a.CachedTokens,
		TotalTokens:     a.TotalTokens,
		Cost:            a.Cost,
		CacheRate:       cacheRateFromTokenTotals(a.EffectiveInput, a.CachedTokens),
	}
	if a.Requests > 0 {
		t.SuccessRate = float64(a.Success) / float64(a.Requests) * 100
	}
	if a.LatencyCount > 0 {
		t.LatencyAvgMs = float64(a.LatencySum) / float64(a.LatencyCount)
	}
	if a.FirstTokenCount > 0 {
		t.FirstTokenAvgMs = float64(a.FirstTokenSum) / float64(a.FirstTokenCount)
	}
	if a.LatencySum > 0 {
		t.OutputTokensPerSecond = float64(a.OutputTokens+a.ReasoningTokens) / (float64(a.LatencySum) / 1000)
	}
	return t
}

// monitorRollupQuery is one aggregate over usage_rollup_buckets. groupCols and
// extraWhere are trusted SQL built in this package, never request input.
type monitorRollupQuery struct {
	filter     MonitorFilter
	kind       string
	from, to   time.Time
	loc        *time.Location
	groupCols  []string
	extraWhere string
	extraArgs  []any
	// limit keeps the heaviest groups first; 0 means no limit.
	limit int
}

type monitorRollupRow struct {
	dims []string
	agg  monitorAgg
}

func runMonitorRollup(q monitorRollupQuery) ([]monitorRollupRow, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	var b strings.Builder
	args := make([]any, 0, 16)
	b.WriteString("SELECT ")
	for _, col := range q.groupCols {
		b.WriteString(col)
		b.WriteString(", ")
	}
	b.WriteString(monitorAggColumns)
	b.WriteString(` FROM usage_rollup_buckets
		WHERE tenant_id = ? AND bucket_kind = ? AND bucket_start >= ? AND bucket_start < ?`)
	args = append(args, q.filter.TenantID, q.kind, rollupKey(q.kind, q.from, q.loc), rollupKey(q.kind, q.to, q.loc))
	predicate, predicateArgs := q.filter.rollupPredicate()
	b.WriteString(predicate)
	args = append(args, predicateArgs...)
	if q.extraWhere != "" {
		b.WriteString(q.extraWhere)
		args = append(args, q.extraArgs...)
	}
	if len(q.groupCols) > 0 {
		b.WriteString(" GROUP BY " + strings.Join(q.groupCols, ", "))
		if q.limit > 0 {
			// Column len(groupCols)+1 is SUM(request_count).
			fmt.Fprintf(&b, " ORDER BY %d DESC LIMIT %d", len(q.groupCols)+1, q.limit)
		}
	}

	rows, err := db.Query(b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("usage: monitor rollup query: %w", err)
	}
	defer rows.Close()
	out := make([]monitorRollupRow, 0)
	for rows.Next() {
		row := monitorRollupRow{dims: make([]string, len(q.groupCols))}
		targets := make([]any, 0, len(q.groupCols)+15)
		for i := range row.dims {
			targets = append(targets, &row.dims[i])
		}
		targets = append(targets, row.agg.scanTargets()...)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("usage: monitor rollup scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// monitorSeriesAggs returns the zero-filled per-step aggregates of the window
// plus their total. Raw rollup buckets are finer than the step on most ranges
// (minutes for 6h, hours for 7d/14d) and are summed into their step here.
func monitorSeriesAggs(w monitorWindow, filter MonitorFilter) ([]monitorAgg, monitorAgg, error) {
	aggs := make([]monitorAgg, w.points())
	var total monitorAgg
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: w.bucketKind, from: w.start, to: w.end, loc: w.loc,
		groupCols: []string{"bucket_start"},
	})
	if err != nil {
		return aggs, total, err
	}
	for _, row := range rows {
		at, ok := parseRollupKey(w.bucketKind, row.dims[0], w.loc)
		if !ok {
			continue
		}
		if idx := w.seriesIndex(at); idx >= 0 {
			aggs[idx].add(row.agg)
			total.add(row.agg)
		}
	}
	return aggs, total, nil
}

func monitorSeriesPoints(w monitorWindow, aggs []monitorAgg) []MonitorSeriesPoint {
	points := make([]MonitorSeriesPoint, len(aggs))
	for i, agg := range aggs {
		points[i] = MonitorSeriesPoint{Start: w.edges[i].Format(time.RFC3339), MonitorTotals: agg.totals()}
	}
	return points
}

func monitorPeriodTotal(w monitorWindow, filter MonitorFilter, from, to time.Time) (monitorAgg, error) {
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: w.bucketKind, from: from, to: to, loc: w.loc,
	})
	if err != nil || len(rows) == 0 {
		return monitorAgg{}, err
	}
	return rows[0].agg, nil
}

// monitorHeatmap folds hour buckets into weekday x hour cells.
func monitorHeatmap(w monitorWindow, filter MonitorFilter) (MonitorHeatmap, error) {
	start, end, days := w.heatmapWindow()
	out := MonitorHeatmap{Start: start.Format(time.RFC3339), Days: days, Cells: []MonitorHeatmapCell{}}
	rows, err := runMonitorRollup(monitorRollupQuery{
		filter: filter, kind: rollupBucketHour, from: start, to: end, loc: w.loc,
		groupCols: []string{"bucket_start"},
	})
	if err != nil {
		return out, err
	}
	var cells [7][24]MonitorHeatmapCell
	for _, row := range rows {
		at, ok := parseRollupKey(rollupBucketHour, row.dims[0], w.loc)
		if !ok {
			continue
		}
		cell := &cells[at.Weekday()][at.Hour()]
		cell.Requests += row.agg.Requests
		cell.Failed += row.agg.Failed
	}
	for weekday := range 7 {
		for hour := range 24 {
			cell := cells[weekday][hour]
			if cell.Requests == 0 {
				continue
			}
			cell.Weekday, cell.Hour = weekday, hour
			out.Cells = append(out.Cells, cell)
		}
	}
	return out, nil
}

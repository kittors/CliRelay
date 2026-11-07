package usage

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Per-model generation-speed and first-token percentiles for the model ranking.
//
// The rollup keeps sums, which give a window's mean but not its median, and a
// mean is easily dragged: a few replies that stall between chunks, or a first
// token stuck behind a long queue, move it far from the typical request.
// Percentiles need every request, so like the latency card they read
// request_logs, count the values into bins in SQL and interpolate inside the
// bin that holds the rank (monitorBinQuantile). Each measurement costs one scan
// of the window's detail rows and returns at most a few hundred bins per model
// however many requests there are; pulling every value into Go to sort it
// would grow with the traffic instead. The percentiles only cover the detail
// retention, which the model ranking reports as performance_coverage_start.

const (
	// Speeds are binned in thousandths of a token per second, so that SQLite
	// and PostgreSQL compute them with the same integer arithmetic; 8 digits
	// reach 100 000 tokens per second.
	monitorSpeedBinDigits = 8
	// First-token times are binned in milliseconds; 7 digits reach 10 000 s.
	monitorFirstTokenBinDigits = 7
)

// monitorTwoDigitBinSQL renders a CASE that bins a non-negative integer by its
// two leading digits: [0, 100) in steps of one, [100, 1000) in steps of ten,
// and so on up to 10^digits, past which everything shares the top bin. Bins are
// at most 10% wide at any magnitude and their numbers grow with the value, so a
// percentile interpolated inside one stays within a few percent of the exact
// one even when a large sample falls into a single bin. expr is trusted SQL
// from this package.
func monitorTwoDigitBinSQL(expr string, digits int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CASE WHEN %s < 100 THEN %s", expr, expr)
	limit, step := int64(1000), int64(10)
	for d := 3; d <= digits; d++ {
		fmt.Fprintf(&b, " WHEN %s < %d THEN %d + %s / %d", expr, limit, (d-2)*100, expr, step)
		limit *= 10
		step *= 10
	}
	fmt.Fprintf(&b, " ELSE %d END", (digits-1)*100)
	return b.String()
}

// monitorModelBins collects one model's binned measurements. The bins reuse
// monitorLatencyBin; its sum stays unused here.
type monitorModelBins struct {
	speed      map[int]monitorLatencyBin
	firstToken map[int]monitorLatencyBin
	// firstTokenWithInputMs and inputTokens sum the first-token streams that
	// reported their input, for the time per thousand input tokens.
	firstTokenWithInputMs int64
	inputTokens           int64
}

func newMonitorModelBins() *monitorModelBins {
	return &monitorModelBins{speed: map[int]monitorLatencyBin{}, firstToken: map[int]monitorLatencyBin{}}
}

// addMonitorBin merges one SQL bin into bins. Model names are trimmed like the
// rollup's, so two spellings of one name can bring the same bin twice.
func addMonitorBin(bins map[int]monitorLatencyBin, index int, bin monitorLatencyBin) {
	if have, ok := bins[index]; ok {
		bin.count += have.count
		bin.min = min(bin.min, have.min)
		bin.max = max(bin.max, have.max)
	}
	bins[index] = bin
}

func (m *monitorModelBins) performance() *MonitorModelPerformance {
	out := &MonitorModelPerformance{
		GenerationSpeed: monitorQuantilesOf(m.speed, 1.0/1000),
		FirstTokenMs:    monitorQuantilesOf(m.firstToken, 1),
	}
	if m.inputTokens > 0 {
		perThousand := float64(m.firstTokenWithInputMs) * 1000 / float64(m.inputTokens)
		out.FirstTokenMsPer1KInput = &perThousand
	}
	return out
}

// monitorQuantilesOf orders sparse bins by their number, which follows the
// value, and reads P50 and P90 off them; scale converts the binned unit.
func monitorQuantilesOf(bins map[int]monitorLatencyBin, scale float64) MonitorQuantiles {
	indexes := make([]int, 0, len(bins))
	for index := range bins {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	ordered := make([]monitorLatencyBin, 0, len(indexes))
	var total int64
	for _, index := range indexes {
		ordered = append(ordered, bins[index])
		total += bins[index].count
	}
	if total == 0 {
		return MonitorQuantiles{}
	}
	return MonitorQuantiles{
		Samples: total,
		P50:     monitorBinQuantile(ordered, total, 0.50) * scale,
		P90:     monitorBinQuantile(ordered, total, 0.90) * scale,
	}
}

// monitorModelPerformance bins every model's generation speeds and first-token
// times over [from, to), and returns the oldest detail row it saw.
func monitorModelPerformance(filter MonitorFilter, from, to time.Time) (map[string]*monitorModelBins, time.Time, error) {
	out := make(map[string]*monitorModelBins)
	db := getReadDB()
	if db == nil {
		return out, time.Time{}, nil
	}
	model := func(name string) *monitorModelBins {
		name = strings.TrimSpace(name)
		if out[name] == nil {
			out[name] = newMonitorModelBins()
		}
		return out[name]
	}
	predicate, predicateArgs := filter.detailPredicate()
	args := append([]any{filter.TenantID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)}, predicateArgs...)
	var oldest time.Time
	seen := func(first storedTime) {
		if first.Valid && (oldest.IsZero() || first.Time.Before(oldest)) {
			oldest = first.Time
		}
	}

	// The speed is computed in the inner query so the bin expression can name
	// it; the sample predicate keeps the divisor at generationMinMs or more.
	speedRows, err := db.Query(`SELECT model, `+monitorTwoDigitBinSQL("speed_milli", monitorSpeedBinDigits)+`,
			COUNT(*), MIN(speed_milli), MAX(speed_milli), MIN(logged_at)
		FROM (
			SELECT model, timestamp AS logged_at,
				output_tokens * 1000000 / (latency_ms - first_token_ms) AS speed_milli
			FROM request_logs
			WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?`+requestLogGenerationSampleSQL+predicate+`
		) samples
		GROUP BY 1, 2`, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("usage: monitor generation speed: %w", err)
	}
	defer speedRows.Close()
	for speedRows.Next() {
		var name string
		var index int
		var bin monitorLatencyBin
		var first storedTime
		if err := speedRows.Scan(&name, &index, &bin.count, &bin.min, &bin.max, &first); err != nil {
			return nil, time.Time{}, fmt.Errorf("usage: monitor generation speed scan: %w", err)
		}
		addMonitorBin(model(name).speed, index, bin)
		seen(first)
	}
	if err := speedRows.Err(); err != nil {
		return nil, time.Time{}, err
	}

	// Same streams as the latency card's first-token histogram, per model.
	firstRows, err := db.Query(`SELECT model, `+monitorTwoDigitBinSQL("first_token_ms", monitorFirstTokenBinDigits)+`,
			COUNT(*), MIN(first_token_ms), MAX(first_token_ms),
			COALESCE(SUM(CASE WHEN input_tokens > 0 THEN first_token_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN input_tokens > 0 THEN input_tokens ELSE 0 END), 0),
			MIN(timestamp)
		FROM request_logs
		WHERE tenant_id = ? AND timestamp >= ? AND timestamp < ?
			AND failed = 0 AND streaming = 1 AND first_token_ms > 0`+predicate+`
		GROUP BY 1, 2`, args...)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("usage: monitor first token: %w", err)
	}
	defer firstRows.Close()
	for firstRows.Next() {
		var name string
		var index int
		var bin monitorLatencyBin
		var withInputMs, inputTokens int64
		var first storedTime
		if err := firstRows.Scan(&name, &index, &bin.count, &bin.min, &bin.max, &withInputMs, &inputTokens, &first); err != nil {
			return nil, time.Time{}, fmt.Errorf("usage: monitor first token scan: %w", err)
		}
		m := model(name)
		addMonitorBin(m.firstToken, index, bin)
		m.firstTokenWithInputMs += withInputMs
		m.inputTokens += inputTokens
		seen(first)
	}
	if err := firstRows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	return out, oldest, nil
}

// attachMonitorModelPerformance gives every model row its per-request
// distributions. A model without detail samples still gets them, with zero
// counts, so the panel can tell "no sample in the details" from "details not
// read". On error the rows are left without any.
func attachMonitorModelPerformance(models *MonitorBreakdown, filter MonitorFilter, w monitorWindow) error {
	bins, oldest, err := monitorModelPerformance(filter, w.start, w.end)
	if err != nil {
		return err
	}
	for i := range models.Rows {
		row := &models.Rows[i]
		m := bins[row.Key]
		if m == nil {
			m = newMonitorModelBins()
		}
		row.Performance = m.performance()
	}
	if !oldest.IsZero() {
		models.PerformanceCoverageStart = oldest.In(w.loc).Format(time.RFC3339)
	}
	return nil
}

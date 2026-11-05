package usage

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

// Bins must follow the value and stay at most 10% wide, or the interpolated
// percentile drifts from the exact one.
func TestMonitorTwoDigitBinSQLOrdersAndBoundsBins(t *testing.T) {
	initMonitorTestDB(t)
	rows, err := getDB().Query(`
		WITH RECURSIVE s(v) AS (
			SELECT 0 UNION ALL SELECT v + 1 + v / 97 FROM s WHERE v < 300000000
		)
		SELECT v, ` + monitorTwoDigitBinSQL("v", monitorSpeedBinDigits) + ` FROM s`)
	if err != nil {
		t.Fatalf("bin query: %v", err)
	}
	defer rows.Close()
	type span struct{ min, max int64 }
	spans := map[int]*span{}
	previous := -1
	for rows.Next() {
		var v int64
		var bin int
		if err := rows.Scan(&v, &bin); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if bin < previous {
			t.Fatalf("bin of %d = %d, below the previous value's %d", v, bin, previous)
		}
		previous = bin
		if s := spans[bin]; s == nil {
			spans[bin] = &span{min: v, max: v}
		} else {
			s.max = v
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	top := (monitorSpeedBinDigits - 1) * 100
	if s := spans[top]; s == nil || s.min < 100000000 {
		t.Fatalf("top bin = %+v, want everything from 10^8 up", s)
	}
	for bin, s := range spans {
		switch {
		case bin == top:
		case s.min < 100 && s.max != s.min:
			t.Fatalf("bin %d holds %d..%d, want one value per bin below 100", bin, s.min, s.max)
		case float64(s.max) > float64(s.min)*1.1:
			t.Fatalf("bin %d holds %d..%d, wider than 10%%", bin, s.min, s.max)
		}
	}
}

// performanceEvents: gen-model has two speed samples (150 and 200 tok/s) and
// four first-token samples (7, 8, 9 and 9 s); the rest must not count.
func performanceEvents(at time.Time) []monitorTestEvent {
	return []monitorTestEvent{
		{model: "gen-model", channel: "A", streaming: true, latency: 10000, ttft: 8000, at: at,
			tokens: TokenStats{InputTokens: 100000, OutputTokens: 300, ReasoningTokens: 5000, TotalTokens: 105300}},
		{model: "gen-model", channel: "B", streaming: true, latency: 12000, ttft: 9000, at: at.Add(time.Second),
			tokens: TokenStats{InputTokens: 200000, OutputTokens: 600, TotalTokens: 200600}},
		// Too little output for a speed; a first token without input.
		{model: "gen-model", channel: "B", streaming: true, latency: 7300, ttft: 7000, at: at.Add(2 * time.Second),
			tokens: TokenStats{OutputTokens: 20, TotalTokens: 20}},
		// First token at the very end: no generation time.
		{model: "gen-model", channel: "B", streaming: true, latency: 9000, ttft: 9000, at: at.Add(3 * time.Second),
			tokens: TokenStats{InputTokens: 100000, OutputTokens: 800, TotalTokens: 100800}},
		{model: "gen-model", channel: "B", failed: true, streaming: true, latency: 3000, ttft: 1000, at: at.Add(4 * time.Second),
			tokens: TokenStats{InputTokens: 100000, OutputTokens: 900, TotalTokens: 100900}},
		// Answered in one piece: nothing to measure.
		{model: "plain-model", latency: 4000, at: at.Add(-time.Minute),
			tokens: TokenStats{InputTokens: 10, OutputTokens: 500, TotalTokens: 510}},
		// Its detail row already expired; only the rollup knows it.
		{model: "expired-model", streaming: true, latency: 3000, ttft: 1000, at: at, rollupOnly: true,
			tokens: TokenStats{InputTokens: 10, OutputTokens: 400, TotalTokens: 410}},
	}
}

func monitorModelRows(t *testing.T, filter MonitorFilter) (MonitorOverview, map[string]MonitorBreakdownRow) {
	t.Helper()
	got, err := QueryMonitorOverview(filter, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	rows := map[string]MonitorBreakdownRow{}
	for _, row := range got.Models.Rows {
		if row.Performance == nil {
			t.Fatalf("model %q has no performance", row.Key)
		}
		rows[row.Key] = row
	}
	return got, rows
}

func assertQuantiles(t *testing.T, name string, got, want MonitorQuantiles) {
	t.Helper()
	if got.Samples != want.Samples || math.Abs(got.P50-want.P50) > 1e-9 || math.Abs(got.P90-want.P90) > 1e-9 {
		t.Fatalf("%s = %+v, want %+v", name, got, want)
	}
}

func TestQueryMonitorOverviewModelPerformance(t *testing.T) {
	initMonitorTestDB(t)
	at := monitorTestNow.Add(-time.Hour)
	for _, ev := range performanceEvents(at) {
		seedMonitorEvent(t, ev)
	}
	got, rows := monitorModelRows(t, MonitorFilter{})

	gen := rows["gen-model"].Performance
	// One value per bin is exact: P50 and P90 interpolate between 150 and 200.
	assertQuantiles(t, "gen-model speed", gen.GenerationSpeed, MonitorQuantiles{Samples: 2, P50: 175, P90: 195})
	assertQuantiles(t, "gen-model first token", gen.FirstTokenMs, MonitorQuantiles{Samples: 4, P50: 8500, P90: 9000})
	// (8 + 9 + 9 s) over 400k input tokens; the stream without input is left out.
	if gen.FirstTokenMsPer1KInput == nil || math.Abs(*gen.FirstTokenMsPer1KInput-65) > 1e-9 {
		t.Fatalf("gen-model first token per 1k input = %v, want 65", gen.FirstTokenMsPer1KInput)
	}
	if speed := rows["gen-model"].GenerationTokensPerSecond; speed == nil || math.Abs(*speed-175) > 1e-9 {
		t.Fatalf("gen-model rollup speed = %v, want the mean 175", speed)
	}

	plain := rows["plain-model"].Performance
	assertQuantiles(t, "plain-model speed", plain.GenerationSpeed, MonitorQuantiles{})
	assertQuantiles(t, "plain-model first token", plain.FirstTokenMs, MonitorQuantiles{})
	if plain.FirstTokenMsPer1KInput != nil {
		t.Fatalf("plain-model first token per 1k input = %v, want null", *plain.FirstTokenMsPer1KInput)
	}

	// Without details the percentiles are empty while the rollup mean stays.
	expired := rows["expired-model"]
	assertQuantiles(t, "expired-model speed", expired.Performance.GenerationSpeed, MonitorQuantiles{})
	if expired.GenerationTokensPerSecond == nil || *expired.GenerationTokensPerSecond != 200 {
		t.Fatalf("expired-model rollup speed = %v, want 200", expired.GenerationTokensPerSecond)
	}

	if want := at.In(monitorTestZone).Format(time.RFC3339); got.Models.PerformanceCoverageStart != want {
		t.Fatalf("performance coverage start = %q, want %q", got.Models.PerformanceCoverageStart, want)
	}
	if got.Channels.PerformanceCoverageStart != "" || got.Consumers.PerformanceCoverageStart != "" {
		t.Fatalf("only the model ranking reports performance coverage")
	}
}

func TestMonitorModelPerformanceFollowsFilters(t *testing.T) {
	initMonitorTestDB(t)
	for _, ev := range performanceEvents(monitorTestNow.Add(-time.Hour)) {
		seedMonitorEvent(t, ev)
	}
	// Channels match case-insensitively, as everywhere else in the monitor.
	_, rows := monitorModelRows(t, MonitorFilter{Channels: []string{"a"}})
	gen := rows["gen-model"].Performance
	assertQuantiles(t, "channel A speed", gen.GenerationSpeed, MonitorQuantiles{Samples: 1, P50: 150, P90: 150})
	assertQuantiles(t, "channel A first token", gen.FirstTokenMs, MonitorQuantiles{Samples: 1, P50: 8000, P90: 8000})
	if _, ok := rows["plain-model"]; ok {
		t.Fatalf("plain-model has no request on channel A")
	}
}

func exactQuantile(values []float64, q float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	pos := q * float64(len(sorted)-1)
	lower, upper := int(math.Floor(pos)), int(math.Ceil(pos))
	return sorted[lower] + (pos-float64(lower))*(sorted[upper]-sorted[lower])
}

// Many samples share each bin; the interpolated percentiles must stay close to
// the exact ones (numpy's default linear interpolation).
func TestMonitorModelPerformanceApproximatesLargeSamples(t *testing.T) {
	initMonitorTestDB(t)
	seedMonitorEvent(t, monitorTestEvent{model: "bulk-model", streaming: true, latency: 3000, ttft: 1000,
		at: monitorTestNow.Add(-2 * time.Hour), rollupOnly: true, tokens: TokenStats{OutputTokens: 400}})

	rng := rand.New(rand.NewPCG(7, 11))
	const n = 3000
	speeds := make([]float64, 0, n)
	firstTokens := make([]float64, 0, n)
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := range n {
		generationMs := 500 + rng.Int64N(30000)
		// Log-normal around 120 tok/s, like a real model's spread.
		speed := math.Exp(math.Log(120) + 0.6*rng.NormFloat64())
		output := max(int64(speed*float64(generationMs)/1000), generationMinOutputTokens)
		firstToken := 200 + rng.Int64N(20000)
		if _, err := tx.Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, model, failed, streaming, latency_ms, first_token_ms, input_tokens, output_tokens)
			VALUES (?, ?, 'bulk-model', 0, 1, ?, ?, 1000, ?)
		`, systemTenantID, monitorTestNow.Add(-2*time.Hour+time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano),
			firstToken+generationMs, firstToken, output); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert: %v", err)
		}
		// SQL truncates the speed to a thousandth of a token per second.
		speeds = append(speeds, float64(output*1000000/generationMs)/1000)
		firstTokens = append(firstTokens, float64(firstToken))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	_, rows := monitorModelRows(t, MonitorFilter{})
	perf := rows["bulk-model"].Performance
	if perf.GenerationSpeed.Samples != n || perf.FirstTokenMs.Samples != n {
		t.Fatalf("samples = %d / %d, want %d", perf.GenerationSpeed.Samples, perf.FirstTokenMs.Samples, n)
	}
	for _, check := range []struct {
		name   string
		got    float64
		values []float64
		q      float64
	}{
		{"speed p50", perf.GenerationSpeed.P50, speeds, 0.5},
		{"speed p90", perf.GenerationSpeed.P90, speeds, 0.9},
		{"first token p50", perf.FirstTokenMs.P50, firstTokens, 0.5},
		{"first token p90", perf.FirstTokenMs.P90, firstTokens, 0.9},
	} {
		want := exactQuantile(check.values, check.q)
		t.Logf("%s = %.3f, exact %.3f", check.name, check.got, want)
		if math.Abs(check.got-want)/want > 0.01 {
			t.Fatalf("%s = %.3f, exact %.3f: more than 1%% off", check.name, check.got, want)
		}
	}
}

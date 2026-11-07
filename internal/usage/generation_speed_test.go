package usage

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type generationCase struct {
	name                            string
	failed, streaming               bool
	latencyMs, firstTokenMs, output int64
	// wantMs is the time after the first token; 0 means "not a sample".
	wantMs int64
}

var generationBoundaryCases = []generationCase{
	{name: "regular stream", streaming: true, latencyMs: 3000, firstTokenMs: 1000, output: 400, wantMs: 2000},
	{name: "first token at the very end", streaming: true, latencyMs: 5000, firstTokenMs: 5000, output: 400},
	{name: "first token after the end", streaming: true, latencyMs: 4000, firstTokenMs: 5000, output: 400},
	{name: "generation just under the floor", streaming: true, latencyMs: 1499, firstTokenMs: 1000, output: 400},
	{name: "generation at the floor", streaming: true, latencyMs: 1500, firstTokenMs: 1000, output: 400, wantMs: 500},
	{name: "output just under the floor", streaming: true, latencyMs: 3000, firstTokenMs: 1000, output: 49},
	{name: "output at the floor", streaming: true, latencyMs: 3000, firstTokenMs: 1000, output: 50, wantMs: 2000},
	{name: "no output", streaming: true, latencyMs: 3000, firstTokenMs: 1000, output: 0},
	// first_token_ms 0 means "not recorded": latency_ms would count the wait.
	{name: "first token not recorded", streaming: true, latencyMs: 30000, firstTokenMs: 0, output: 400},
	{name: "not streamed", latencyMs: 3000, firstTokenMs: 1000, output: 400},
	{name: "failed stream", failed: true, streaming: true, latencyMs: 3000, firstTokenMs: 1000, output: 400},
}

func TestGenerationSampleBoundaries(t *testing.T) {
	for _, tc := range generationBoundaryCases {
		t.Run(tc.name, func(t *testing.T) {
			gotMs, ok := generationSample(tc.failed, tc.streaming, tc.latencyMs, tc.firstTokenMs, tc.output)
			if ok != (tc.wantMs > 0) || gotMs != tc.wantMs {
				t.Fatalf("generationSample = (%d, %v), want (%d, %v)", gotMs, ok, tc.wantMs, tc.wantMs > 0)
			}
			totals := requestGenerationTotals(tc.failed, tc.streaming, tc.latencyMs, tc.firstTokenMs, tc.output)
			if tc.wantMs == 0 {
				if totals != (generationTotals{}) {
					t.Fatalf("non-sample contributed %+v", totals)
				}
				return
			}
			want := generationTotals{
				Count: 1, OutputTokens: tc.output, SumMs: tc.wantMs,
				SpeedSum: float64(tc.output) * 1000 / float64(tc.wantMs),
			}
			if totals != want {
				t.Fatalf("requestGenerationTotals = %+v, want %+v", totals, want)
			}
		})
	}
}

// The percentiles select samples in SQL and the rollup selects them in Go; both
// must keep exactly the same requests.
func TestRequestLogGenerationSampleSQLMatchesGo(t *testing.T) {
	initMonitorTestDB(t)
	for i, tc := range generationBoundaryCases {
		failed, streaming := 0, 0
		if tc.failed {
			failed = 1
		}
		if tc.streaming {
			streaming = 1
		}
		if _, err := getDB().Exec(`
			INSERT INTO request_logs (tenant_id, timestamp, model, failed, streaming, latency_ms, first_token_ms, output_tokens)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, systemTenantID, monitorTestNow.Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano),
			tc.name, failed, streaming, tc.latencyMs, tc.firstTokenMs, tc.output); err != nil {
			t.Fatalf("insert %s: %v", tc.name, err)
		}
	}
	rows, err := getDB().Query(`SELECT model FROM request_logs WHERE 1 = 1` + requestLogGenerationSampleSQL)
	if err != nil {
		t.Fatalf("query samples: %v", err)
	}
	defer rows.Close()
	selected := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		selected[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, tc := range generationBoundaryCases {
		if selected[tc.name] != (tc.wantMs > 0) {
			t.Fatalf("%s: SQL selects it = %v, Go says sample = %v", tc.name, selected[tc.name], tc.wantMs > 0)
		}
	}
}

func TestGenerationTotalsUsageIsPerRequestMean(t *testing.T) {
	var none generationTotals
	if got := none.usage(); got.GenerationTokensPerSecond != nil || got.legacyOutputTokensPerSecond() != 0 {
		t.Fatalf("no sample: speed = %v, legacy = %v, want null and 0", got.GenerationTokensPerSecond, got.legacyOutputTokensPerSecond())
	}

	// 400 tokens in 2 s (200 tok/s) and 1000 tokens in 10 s (100 tok/s): the
	// mean per request is 150, total output over total time only 116.7.
	totals := requestGenerationTotals(false, true, 3000, 1000, 400)
	totals.add(requestGenerationTotals(false, true, 11000, 1000, 1000))
	got := totals.usage()
	if got.GenerationSamples != 2 || got.GenerationOutputTokens != 1400 || got.GenerationMs != 12000 {
		t.Fatalf("sums = %+v", got)
	}
	if got.GenerationTokensPerSecond == nil || math.Abs(*got.GenerationTokensPerSecond-150) > 1e-9 {
		t.Fatalf("speed = %v, want 150", got.GenerationTokensPerSecond)
	}
	if got.legacyOutputTokensPerSecond() != *got.GenerationTokensPerSecond {
		t.Fatalf("legacy field = %v, want the generation speed", got.legacyOutputTokensPerSecond())
	}
}

func readRollupGeneration(t *testing.T, db *sql.DB) map[string]generationTotals {
	t.Helper()
	rows, err := db.Query(`
		SELECT bucket_kind, SUM(generation_count), SUM(generation_output_tokens),
		       SUM(generation_sum_ms), SUM(generation_speed_sum)
		FROM usage_rollup_buckets GROUP BY bucket_kind`)
	if err != nil {
		t.Fatalf("read rollup generation: %v", err)
	}
	defer rows.Close()
	out := map[string]generationTotals{}
	for rows.Next() {
		var kind string
		var g generationTotals
		if err := rows.Scan(&kind, &g.Count, &g.OutputTokens, &g.SumMs, &g.SpeedSum); err != nil {
			t.Fatalf("scan rollup generation: %v", err)
		}
		out[kind] = g
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup generation rows: %v", err)
	}
	return out
}

// mixedGenerationEvents are two samples (200 and 100 tok/s) among requests
// that must not count: a huge reasoning count, a first token at the very end,
// too little output, a reply that was not streamed and a failure.
func mixedGenerationEvents(at time.Time) []monitorTestEvent {
	return []monitorTestEvent{
		{model: "gen-model", streaming: true, latency: 3000, ttft: 1000, at: at,
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 400, ReasoningTokens: 50000, TotalTokens: 51400}},
		{model: "gen-model", streaming: true, latency: 11000, ttft: 1000, at: at.Add(time.Second),
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000}},
		{model: "gen-model", streaming: true, latency: 9000, ttft: 9000, at: at.Add(2 * time.Second),
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 800, TotalTokens: 1800}},
		{model: "gen-model", streaming: true, latency: 3000, ttft: 1000, at: at.Add(3 * time.Second),
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 20, TotalTokens: 1020}},
		{model: "gen-model", latency: 3000, at: at.Add(4 * time.Second),
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 900, TotalTokens: 1900}},
		{model: "gen-model", failed: true, streaming: true, latency: 3000, ttft: 1000, at: at.Add(5 * time.Second),
			tokens: TokenStats{InputTokens: 1000, OutputTokens: 900, TotalTokens: 1900}},
	}
}

func TestProjectUsageRollupWritesGenerationColumns(t *testing.T) {
	initMonitorTestDB(t)
	for _, ev := range mixedGenerationEvents(monitorTestNow.Add(-time.Hour)) {
		seedMonitorEvent(t, ev)
	}
	want := generationTotals{Count: 2, OutputTokens: 1400, SumMs: 12000, SpeedSum: 300}
	got := readRollupGeneration(t, getDB())
	for _, kind := range []string{rollupBucketMinute, rollupBucketQuotaMinuteUTC, rollupBucketHour, rollupBucketDay, rollupBucketLifetime} {
		if got[kind] != want {
			t.Fatalf("%s generation = %+v, want %+v", kind, got[kind], want)
		}
	}
}

// The rebuild from request_logs goes through the same projection, so it ends
// with the generation columns the live writes left.
func TestUsageRollupRebuildKeepsGenerationColumns(t *testing.T) {
	initMonitorTestDB(t)
	for _, ev := range mixedGenerationEvents(monitorTestNow.Add(-time.Hour)) {
		seedMonitorEvent(t, ev)
	}
	live := readRollupGeneration(t, getDB())
	if err := setProjectionMarker(getDB(), usageRollupBackfillMarker, rollupMarkerPending); err != nil {
		t.Fatalf("reset marker: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), monitorTestZone, rollupMarkerDone); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := readRollupGeneration(t, getDB())
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt kinds = %v, live = %v", rebuilt, live)
	}
	for kind, want := range live {
		got := rebuilt[kind]
		if got.Count != want.Count || got.OutputTokens != want.OutputTokens || got.SumMs != want.SumMs ||
			math.Abs(got.SpeedSum-want.SpeedSum) > 1e-9 {
			t.Fatalf("%s after rebuild = %+v, live wrote %+v", kind, got, want)
		}
	}
}

func TestQueryMonitorOverviewReportsGenerationSpeed(t *testing.T) {
	initMonitorTestDB(t)
	for _, ev := range mixedGenerationEvents(monitorTestNow.Add(-time.Hour)) {
		seedMonitorEvent(t, ev)
	}
	// A model that is only ever answered in one piece measures no speed.
	seedMonitorEvent(t, monitorTestEvent{model: "batch-model", latency: 4000, at: monitorTestNow.Add(-time.Hour),
		tokens: TokenStats{InputTokens: 10, OutputTokens: 500, TotalTokens: 510}})

	got, err := QueryMonitorOverview(MonitorFilter{}, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	cur := got.Summary.Current
	if cur.GenerationSamples != 2 || cur.GenerationTokensPerSecond == nil || math.Abs(*cur.GenerationTokensPerSecond-150) > 1e-9 {
		t.Fatalf("summary generation = %d samples / %v, want 2 / 150", cur.GenerationSamples, cur.GenerationTokensPerSecond)
	}
	if cur.OutputTokensPerSecond != 150 {
		t.Fatalf("deprecated output_tokens_per_second = %v, want the generation speed", cur.OutputTokensPerSecond)
	}
	// Request counts and tokens keep counting every request.
	if cur.Requests != 7 || cur.OutputTokens != 400+1000+800+20+900+900+500 || cur.ReasoningTokens != 50000 {
		t.Fatalf("summary counts = %+v", cur)
	}

	rows := map[string]MonitorBreakdownRow{}
	for _, row := range got.Models.Rows {
		rows[row.Key] = row
	}
	if speed := rows["gen-model"].GenerationTokensPerSecond; speed == nil || math.Abs(*speed-150) > 1e-9 {
		t.Fatalf("gen-model speed = %v, want 150", speed)
	}
	batch := rows["batch-model"]
	if batch.GenerationTokensPerSecond != nil || batch.GenerationSamples != 0 || batch.OutputTokensPerSecond != 0 {
		t.Fatalf("batch-model = %d samples / %v / legacy %v, want none", batch.GenerationSamples, batch.GenerationTokensPerSecond, batch.OutputTokensPerSecond)
	}

	// The series point of the hour holds the same samples.
	var seriesSamples int64
	for _, point := range got.Series {
		seriesSamples += point.GenerationSamples
	}
	if seriesSamples != 2 {
		t.Fatalf("series samples = %d, want 2", seriesSamples)
	}
}

// A SQLite database from before the generation columns gains them on start;
// the buckets it already had hold no sample and report no speed.
//
// InitDB prunes rollup buckets past retention by time.Now(), while the legacy
// bucket and the queries are pinned to monitorTestNow. Run in a synctest bubble
// whose clock reads monitorTestNow: with the real clock the 2026-10-07 bucket
// fell out of the 30-day hour retention on 2026-11-06 and the test failed from
// then on.
func TestUsageRollupGenerationColumnsMigrateLegacySQLite(t *testing.T) {
	synctest.Test(t, testUsageRollupGenerationColumnsMigrateLegacySQLite)
}

func testUsageRollupGenerationColumnsMigrateLegacySQLite(t *testing.T) {
	time.Sleep(time.Until(monitorTestNow))
	dbPath := filepath.Join(t.TempDir(), "legacy-rollup.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE usage_rollup_buckets (
		  tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
		  bucket_kind TEXT NOT NULL, bucket_start TEXT NOT NULL,
		  api_key_id TEXT NOT NULL DEFAULT '', end_user_id TEXT NOT NULL DEFAULT '',
		  auth_subject_id TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		  source TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '',
		  request_count INTEGER NOT NULL DEFAULT 0, success_count INTEGER NOT NULL DEFAULT 0,
		  failure_count INTEGER NOT NULL DEFAULT 0, streaming_count INTEGER NOT NULL DEFAULT 0,
		  input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		  reasoning_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0,
		  effective_input_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		  cost_total REAL NOT NULL DEFAULT 0,
		  latency_sum_ms INTEGER NOT NULL DEFAULT 0, latency_count INTEGER NOT NULL DEFAULT 0,
		  first_token_sum_ms INTEGER NOT NULL DEFAULT 0, first_token_count INTEGER NOT NULL DEFAULT 0,
		  cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		  cache_known_prompt_tokens INTEGER NOT NULL DEFAULT 0,
		  updated_at DATETIME NOT NULL,
		  PRIMARY KEY (tenant_id, bucket_kind, bucket_start, api_key_id, end_user_id, auth_subject_id, model, source, channel_name)
		);
		INSERT INTO usage_rollup_buckets (bucket_kind, bucket_start, model, request_count, success_count,
		  streaming_count, output_tokens, latency_sum_ms, latency_count, first_token_sum_ms, first_token_count, updated_at)
		VALUES ('hour', '2026-10-07T12', 'old-model', 1, 1, 1, 400, 3000, 1, 1000, 1, '2026-10-07T04:00:00Z');
	`); err != nil {
		t.Fatalf("create legacy rollup: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, monitorTestZone); err != nil {
		t.Fatalf("InitDB over legacy rollup: %v", err)
	}
	stopRequestLogMaintenance()

	got, err := QueryMonitorOverview(MonitorFilter{}, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	cur := got.Summary.Current
	if cur.Requests != 1 || cur.OutputTokens != 400 {
		t.Fatalf("legacy bucket not read: %+v", cur)
	}
	if cur.GenerationSamples != 0 || cur.GenerationTokensPerSecond != nil || cur.OutputTokensPerSecond != 0 {
		t.Fatalf("legacy bucket reports a speed: %d samples / %v / legacy %v", cur.GenerationSamples, cur.GenerationTokensPerSecond, cur.OutputTokensPerSecond)
	}

	// New requests land in the migrated columns.
	seedMonitorEvent(t, monitorTestEvent{model: "old-model", streaming: true, latency: 3000, ttft: 1000,
		at: monitorTestNow.Add(-time.Hour), tokens: TokenStats{OutputTokens: 400, TotalTokens: 400}})
	got, err = QueryMonitorOverview(MonitorFilter{}, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview after write: %v", err)
	}
	cur = got.Summary.Current
	if cur.GenerationSamples != 1 || cur.GenerationTokensPerSecond == nil || *cur.GenerationTokensPerSecond != 200 {
		t.Fatalf("after a new sample: %d samples / %v, want 1 / 200", cur.GenerationSamples, cur.GenerationTokensPerSecond)
	}
}

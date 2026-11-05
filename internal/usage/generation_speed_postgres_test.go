package usage

import (
	"math"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// Production runs on PostgreSQL, where the generation columns come from a
// runtime migration and the percentile SQL (integer speeds, two-digit bins,
// positional GROUP BY over a subquery) runs on another engine than the SQLite
// of the unit tests.
func TestPostgresMonitorGenerationSpeed(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)
	if err := InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()
	truncatePostgresRuntimeTables(t, getDB())
	// The rows are this test's: leave the shared database as it was found. This
	// runs before the CloseDB registered above.
	t.Cleanup(func() { truncatePostgresRuntimeTables(t, getDB()) })

	var columns int
	if err := getDB().QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'usage_rollup_buckets'
		  AND column_name IN ('generation_count', 'generation_output_tokens', 'generation_sum_ms', 'generation_speed_sum')
	`).Scan(&columns); err != nil {
		t.Fatalf("read rollup columns: %v", err)
	}
	if columns != 4 {
		t.Fatalf("generation columns after migration = %d, want 4", columns)
	}

	now := time.Now().UTC()
	// Whole seconds, so the coverage start reads back exactly.
	at := now.Add(-30 * time.Minute).Truncate(time.Second)
	// The rows go through the live write path and its rollup projection.
	insert := func(offset time.Duration, failed, streaming bool, latencyMs, firstTokenMs int64, tokens TokenStats) {
		t.Helper()
		InsertRequestLog(RequestLogEntry{
			APIKey: "sk-gen-pg", APIKeyID: "key-gen-pg", APIKeyName: "Gen", Model: "pg-gen", Source: "gemini",
			Failed: failed, Streaming: streaming, LatencyMs: latencyMs, FirstTokenMs: firstTokenMs,
			Timestamp: at.Add(offset), Tokens: tokens,
		})
	}
	// Two samples at 150 and 200 tok/s; the reasoning count stays out.
	insert(0, false, true, 10000, 8000, TokenStats{InputTokens: 100000, OutputTokens: 300, ReasoningTokens: 5000, TotalTokens: 105300})
	insert(time.Second, false, true, 12000, 9000, TokenStats{InputTokens: 200000, OutputTokens: 600, TotalTokens: 200600})
	// A first token at the very end, a failure and a reply not streamed.
	insert(2*time.Second, false, true, 9000, 9000, TokenStats{InputTokens: 100000, OutputTokens: 800, TotalTokens: 100800})
	insert(3*time.Second, true, true, 3000, 1000, TokenStats{InputTokens: 100000, OutputTokens: 900, TotalTokens: 100900})
	insert(4*time.Second, false, false, 4000, 0, TokenStats{InputTokens: 10, OutputTokens: 500, TotalTokens: 510})
	// A bucket written by a build without the columns holds no sample.
	if _, err := getDB().Exec(`
		INSERT INTO usage_rollup_buckets (tenant_id, bucket_kind, bucket_start, model, request_count,
			success_count, streaming_count, output_tokens, latency_sum_ms, latency_count, updated_at)
		VALUES (?, 'hour', ?, 'pg-old', 1, 1, 1, 400, 3000, 1, ?)
	`, systemTenantID, at.Format("2006-01-02T15"), now); err != nil {
		t.Fatalf("insert legacy bucket: %v", err)
	}

	got, err := QueryMonitorOverview(MonitorFilter{}, MonitorRange24h, now)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	cur := got.Summary.Current
	if cur.GenerationSamples != 2 || cur.GenerationTokensPerSecond == nil || math.Abs(*cur.GenerationTokensPerSecond-175) > 1e-9 {
		t.Fatalf("summary generation = %d samples / %v, want 2 / 175", cur.GenerationSamples, cur.GenerationTokensPerSecond)
	}
	if cur.GenerationOutputTokens != 900 || cur.GenerationMs != 5000 {
		t.Fatalf("summary generation sums = %d tokens / %d ms, want 900 / 5000", cur.GenerationOutputTokens, cur.GenerationMs)
	}

	rows := map[string]MonitorBreakdownRow{}
	for _, row := range got.Models.Rows {
		rows[row.Key] = row
	}
	old := rows["pg-old"]
	if old.GenerationTokensPerSecond != nil || old.OutputTokensPerSecond != 0 {
		t.Fatalf("legacy bucket speed = %v / legacy %v, want none", old.GenerationTokensPerSecond, old.OutputTokensPerSecond)
	}
	perf := rows["pg-gen"].Performance
	if perf == nil {
		t.Fatal("pg-gen has no performance")
	}
	assertQuantiles(t, "pg-gen speed", perf.GenerationSpeed, MonitorQuantiles{Samples: 2, P50: 175, P90: 195})
	assertQuantiles(t, "pg-gen first token", perf.FirstTokenMs, MonitorQuantiles{Samples: 3, P50: 9000, P90: 9000})
	if perf.FirstTokenMsPer1KInput == nil || math.Abs(*perf.FirstTokenMsPer1KInput-65) > 1e-9 {
		t.Fatalf("pg-gen first token per 1k input = %v, want 65", perf.FirstTokenMsPer1KInput)
	}
	if want := at.Format(time.RFC3339); got.Models.PerformanceCoverageStart != want {
		t.Fatalf("performance coverage start = %q, want %q", got.Models.PerformanceCoverageStart, want)
	}
}

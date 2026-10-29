package usage

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// The upstream semantics the cases below stand for. OpenAI, Codex and Gemini
// count cache reads inside their input figure; Anthropic counts neither reads
// nor writes in it.
func openAIStyleTokens(input, read int64) TokenStats {
	return TokenStats{
		InputTokens: input, TotalTokens: input, CachedTokens: read,
		CacheReadTokens: read, CacheReadIncludedInInput: true, CacheReported: true,
	}
}

func anthropicStyleTokens(input, read, write int64) TokenStats {
	cached := read
	if cached == 0 {
		cached = write
	}
	return TokenStats{
		InputTokens: input, TotalTokens: input, CachedTokens: cached,
		CacheReadTokens: read, CacheWriteTokens: write, CacheReported: true,
	}
}

func insertCacheRateRows(t *testing.T, rows []TokenStats) {
	t.Helper()
	if err := UpsertAPIKey(APIKeyRow{ID: "key-cache-rate", Key: "sk-cache-rate", Name: "Cache"}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	for index, row := range rows {
		InsertLogWithDetailsIdentity("sk-cache-rate", "key-cache-rate", "Cache", "claude-opus-5-5", "claude", "Claude", "auth-cache-rate",
			false, now.Add(time.Duration(index)*time.Millisecond), 1, 0, row, "", "", "")
	}
}

func TestCacheHitRateCountsOnlyReportedReads(t *testing.T) {
	testCases := []struct {
		name string
		rows []TokenStats
		// want is nil when no row reported its cache use.
		want       *float64
		wantReads  int64
		wantWrites int64
	}{
		{
			name:      "reads inside the input figure",
			rows:      []TokenStats{openAIStyleTokens(1000, 400)},
			want:      ptrFloat(40),
			wantReads: 400,
		},
		{
			name:      "reads beside a small input figure",
			rows:      []TokenStats{anthropicStyleTokens(21, 188086, 0)},
			want:      ptrFloat(float64(188086) / float64(188107) * 100),
			wantReads: 188086,
		},
		{
			// The first turn of a conversation only writes the cache. It used to
			// count as a near-100% hit.
			name:       "a turn that only wrote the cache is a miss",
			rows:       []TokenStats{anthropicStyleTokens(21, 0, 50000)},
			want:       ptrFloat(0),
			wantWrites: 50000,
		},
		{
			// Writes used to be dropped whenever there were reads.
			name:       "writes count in the prompt next to reads",
			rows:       []TokenStats{anthropicStyleTokens(100, 900, 1000)},
			want:       ptrFloat(45),
			wantReads:  900,
			wantWrites: 1000,
		},
		{
			name: "a conversation: one write, then reads",
			rows: []TokenStats{
				anthropicStyleTokens(10, 0, 5000),
				anthropicStyleTokens(20, 5000, 100),
				anthropicStyleTokens(30, 5100, 200),
			},
			want:       ptrFloat(float64(10100) / float64(10+5000+20+5000+100+30+5100+200) * 100),
			wantReads:  10100,
			wantWrites: 5300,
		},
		{
			// An upstream that says nothing about its cache is left out, not
			// counted as a miss that drags the rate down.
			name:      "requests without cache accounting are left out",
			rows:      []TokenStats{openAIStyleTokens(100, 50), {InputTokens: 900, TotalTokens: 900}},
			want:      ptrFloat(50),
			wantReads: 50,
		},
		{
			// Rows written before reads and writes were stored only carry the
			// legacy cached_tokens; what it held cannot be told apart.
			name: "legacy cached tokens alone are unknown",
			rows: []TokenStats{{InputTokens: 1000, CachedTokens: 400, TotalTokens: 1000}},
			want: nil,
		},
		{
			name: "no traffic",
			rows: nil,
			want: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			initTestUsageDB(t, config.RequestLogStorageConfig{})
			insertCacheRateRows(t, tc.rows)

			check := func(source string, got CacheUsage, legacy float64) {
				t.Helper()
				if !sameRate(got.CacheHitRate, tc.want) {
					t.Fatalf("%s hit rate = %s, want %s", source, formatRate(got.CacheHitRate), formatRate(tc.want))
				}
				wantLegacy := 0.0
				if tc.want != nil {
					wantLegacy = *tc.want
				}
				if math.Abs(legacy-wantLegacy) > 1e-9 {
					t.Fatalf("%s cache_rate = %v, want %v", source, legacy, wantLegacy)
				}
				if got.CacheReadTokens != tc.wantReads || got.CacheWriteTokens != tc.wantWrites {
					t.Fatalf("%s reads/writes = %d/%d, want %d/%d", source, got.CacheReadTokens, got.CacheWriteTokens, tc.wantReads, tc.wantWrites)
				}
			}

			rollupParams := LogQueryParams{APIKeyIDs: []string{"key-cache-rate"}, Days: 7}
			if queryStatsRequiresDetailAggregation(normalizeLogQueryParams(rollupParams)) {
				t.Fatal("expected an API key filter to read the rollups")
			}
			stats, err := QueryStats(rollupParams)
			if err != nil {
				t.Fatalf("QueryStats() error = %v", err)
			}
			check("rollup stats", stats.CacheUsage, stats.CacheRate)

			// A credential filter is not a rollup dimension, so this aggregates
			// request_logs directly.
			detailParams := LogQueryParams{APIKeyIDs: []string{"key-cache-rate"}, AuthIndexes: []string{"auth-cache-rate"}, Days: 7}
			if !queryStatsRequiresDetailAggregation(normalizeLogQueryParams(detailParams)) {
				t.Fatal("expected the auth index filter to aggregate request_logs")
			}
			stats, err = QueryStats(detailParams)
			if err != nil {
				t.Fatalf("QueryStats(details) error = %v", err)
			}
			check("detail stats", stats.CacheUsage, stats.CacheRate)

			kpi, err := QueryDashboardKPI(7)
			if err != nil {
				t.Fatalf("QueryDashboardKPI() error = %v", err)
			}
			check("dashboard", kpi.CacheUsage, kpi.CacheRate)

			chart, err := QueryPublicChartData("sk-cache-rate", 7)
			if err != nil {
				t.Fatalf("QueryPublicChartData() error = %v", err)
			}
			check("public chart", chart.Stats.CacheUsage, chart.Stats.CacheRate)

			overview, err := QueryMonitorOverview(MonitorFilter{}, MonitorRange24h, time.Now())
			if err != nil {
				t.Fatalf("QueryMonitorOverview() error = %v", err)
			}
			check("monitor", overview.Summary.Current.CacheUsage, overview.Summary.Current.CacheRate)
		})
	}
}

func TestRequestLogRowsCarryCacheColumns(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	insertCacheRateRows(t, []TokenStats{
		anthropicStyleTokens(30, 5000, 200),
		openAIStyleTokens(1000, 400),
		{InputTokens: 900, TotalTokens: 900},
	})

	result, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 7})
	if err != nil {
		t.Fatalf("QueryLogs() error = %v", err)
	}
	if len(result.Items) != 3 {
		t.Fatalf("QueryLogs() returned %d rows, want 3", len(result.Items))
	}
	// Newest first.
	unknown, openAI, anthropic := result.Items[0], result.Items[1], result.Items[2]
	if unknown.CacheReadTokens != nil || unknown.CacheWriteTokens != nil {
		t.Fatalf("row without cache accounting = %v/%v, want null reads and writes", unknown.CacheReadTokens, unknown.CacheWriteTokens)
	}
	if unknown.PromptTokens == nil || *unknown.PromptTokens != 900 {
		t.Fatalf("row without cache accounting prompt = %v, want 900", unknown.PromptTokens)
	}
	if openAI.CacheReadTokens == nil || *openAI.CacheReadTokens != 400 || openAI.CacheWriteTokens == nil || *openAI.CacheWriteTokens != 0 {
		t.Fatalf("OpenAI-style row reads/writes = %v/%v, want 400/0", openAI.CacheReadTokens, openAI.CacheWriteTokens)
	}
	if openAI.PromptTokens == nil || *openAI.PromptTokens != 1000 {
		t.Fatalf("OpenAI-style row prompt = %v, want 1000 (reads are inside the input)", openAI.PromptTokens)
	}
	if anthropic.CacheReadTokens == nil || *anthropic.CacheReadTokens != 5000 || anthropic.CacheWriteTokens == nil || *anthropic.CacheWriteTokens != 200 {
		t.Fatalf("Anthropic-style row reads/writes = %v/%v, want 5000/200", anthropic.CacheReadTokens, anthropic.CacheWriteTokens)
	}
	if anthropic.PromptTokens == nil || *anthropic.PromptTokens != 5230 {
		t.Fatalf("Anthropic-style row prompt = %v, want 5230", anthropic.PromptTokens)
	}
	if anthropic.CachedTokens != 5000 {
		t.Fatalf("legacy cached_tokens = %d, want 5000 (reads, else writes)", anthropic.CachedTokens)
	}

	single, err := QueryLogRowByID(anthropic.ID)
	if err != nil {
		t.Fatalf("QueryLogRowByID() error = %v", err)
	}
	if single.CacheReadTokens == nil || *single.CacheReadTokens != 5000 || single.PromptTokens == nil || *single.PromptTokens != 5230 {
		t.Fatalf("QueryLogRowByID() reads/prompt = %v/%v, want 5000/5230", single.CacheReadTokens, single.PromptTokens)
	}

	// Rows written before the columns existed keep NULL and read as unknown.
	if _, err := getDB().Exec(`UPDATE request_logs SET cache_read_tokens = NULL, cache_write_tokens = NULL, prompt_tokens = NULL WHERE id = ?`, anthropic.ID); err != nil {
		t.Fatalf("simulate a legacy row: %v", err)
	}
	legacy, err := QueryLogRowByID(anthropic.ID)
	if err != nil {
		t.Fatalf("QueryLogRowByID(legacy) error = %v", err)
	}
	if legacy.CacheReadTokens != nil || legacy.CacheWriteTokens != nil || legacy.PromptTokens != nil {
		t.Fatalf("legacy row = %v/%v/%v, want all null", legacy.CacheReadTokens, legacy.CacheWriteTokens, legacy.PromptTokens)
	}
}

// A rebuild of the rollups from request_logs must keep the cache columns, and
// rows whose cache use is unknown must stay out of them.
func TestUsageRollupRebuildKeepsCacheColumns(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	insertCacheRateRows(t, []TokenStats{
		anthropicStyleTokens(30, 5000, 200),
		{InputTokens: 900, TotalTokens: 900},
	})
	before, err := QueryStats(LogQueryParams{APIKeyIDs: []string{"key-cache-rate"}, Days: 7})
	if err != nil {
		t.Fatalf("QueryStats() error = %v", err)
	}

	if _, err := getDB().Exec(`DELETE FROM usage_rollup_buckets`); err != nil {
		t.Fatalf("clear rollup: %v", err)
	}
	if _, err := getDB().Exec(`DELETE FROM usage_projection_markers WHERE marker_key = ?`, usageRollupBackfillMarker); err != nil {
		t.Fatalf("clear marker: %v", err)
	}
	if err := runUsageRollupBackfillAtInitDB(getDB(), time.UTC, rollupMarkerDone); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	after, err := QueryStats(LogQueryParams{APIKeyIDs: []string{"key-cache-rate"}, Days: 7})
	if err != nil {
		t.Fatalf("QueryStats() after rebuild error = %v", err)
	}
	if !sameRate(after.CacheHitRate, before.CacheHitRate) || after.CacheReadTokens != 5000 || after.CacheWriteTokens != 200 {
		t.Fatalf("after rebuild = %+v, want the live rollup %+v", after.CacheUsage, before.CacheUsage)
	}
	if want := float64(5000) / float64(5230) * 100; !sameRate(after.CacheHitRate, &want) {
		t.Fatalf("after rebuild hit rate = %s, want %v", formatRate(after.CacheHitRate), want)
	}
}

func TestPromptTokenTotal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens TokenStats
		want   int64
	}{
		{"reads inside the input", openAIStyleTokens(1000, 400), 1000},
		{"reads and writes beside the input", anthropicStyleTokens(10, 300, 200), 510},
		{"an upstream claiming more reads than input", TokenStats{InputTokens: 100, CacheReadTokens: 150, CacheReadIncludedInInput: true}, 150},
		{"no cache", TokenStats{InputTokens: 42}, 42},
		{"negative counters are ignored", TokenStats{InputTokens: -5, CacheReadTokens: -1, CacheWriteTokens: -2}, 0},
	} {
		if got := promptTokenTotal(tc.tokens); got != tc.want {
			t.Errorf("%s: promptTokenTotal() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRequestLogSessionIDSurvivesSkippedContent(t *testing.T) {
	// A one-megabyte cap: a row bigger than that keeps no content at all.
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true, ContentRetentionDays: 30, MaxTotalSizeMB: 1})
	input := makePseudoRandomText(2 << 20)
	InsertLogWithDetailsIdentity("sk-session", "", "", "gpt-5.5", "codex", "Codex", "auth-1", false, time.Now().UTC(), 1, 0,
		TokenStats{InputTokens: 10, TotalTokens: 10}, input, "", `{"client":{"headers":{"Session_id":["session-large"]}}}`)

	var contentRows int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_log_content`).Scan(&contentRows); err != nil {
		t.Fatalf("count content rows: %v", err)
	}
	if contentRows != 0 {
		t.Fatalf("content rows = %d, want 0 (over the size cap)", contentRows)
	}
	var sessionID string
	if err := getDB().QueryRow(`SELECT session_id FROM request_logs`).Scan(&sessionID); err != nil {
		t.Fatalf("read session_id: %v", err)
	}
	if sessionID != "session-large" {
		t.Fatalf("request_logs.session_id = %q, want session-large", sessionID)
	}
}

func TestRequestLogSessionBackfillFillsOlderRows(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true, ContentRetentionDays: 30})
	restore := shortenSessionBackfillPauses()
	defer restore()

	now := time.Now().UTC()
	detail := func(session string) string { return `{"client":{"headers":{"X-Session-Id":["` + session + `"]}}}` }
	for i, session := range []string{"session-a", "session-a", "session-b", ""} {
		InsertLogWithDetailsIdentity("sk-backfill", "", "", "gpt-5.5", "codex", "Codex", "auth-1", false, now.Add(time.Duration(i)*time.Second), 1, 0,
			TokenStats{InputTokens: 10, TotalTokens: 10}, "{}", "{}", detail(session))
	}
	// Rows written before request_logs had the column, plus one whose session
	// was already set and must not be overwritten.
	if _, err := getDB().Exec(`UPDATE request_logs SET session_id = ''`); err != nil {
		t.Fatalf("simulate older rows: %v", err)
	}
	var keptID int64
	if err := getDB().QueryRow(`SELECT MIN(id) FROM request_logs`).Scan(&keptID); err != nil {
		t.Fatalf("pick a row: %v", err)
	}
	if _, err := getDB().Exec(`UPDATE request_logs SET session_id = 'kept' WHERE id = ?`, keptID); err != nil {
		t.Fatalf("set a session: %v", err)
	}

	// A pass that keeps finding rows never ends; the deadline turns that into a
	// failure instead of a hung test.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runRequestLogSessionBackfill(ctx, getDB()); err != nil {
		t.Fatalf("runRequestLogSessionBackfill() error = %v", err)
	}
	rows, err := getDB().Query(`SELECT session_id FROM request_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	var got []string
	for rows.Next() {
		var session string
		if err := rows.Scan(&session); err != nil {
			t.Fatalf("scan session: %v", err)
		}
		got = append(got, session)
	}
	_ = rows.Close()
	want := []string{"kept", "session-a", "session-b", ""}
	if len(got) != len(want) {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sessions = %v, want %v", got, want)
		}
	}
	if !requestLogSessionBackfillCompleted(getDB()) {
		t.Fatal("backfill marker not written")
	}

	// Once done the pass never runs again, even if a row lost its session.
	if _, err := getDB().Exec(`UPDATE request_logs SET session_id = '' WHERE session_id = 'session-b'`); err != nil {
		t.Fatalf("clear a session: %v", err)
	}
	if err := runRequestLogSessionBackfill(ctx, getDB()); err != nil {
		t.Fatalf("second run error = %v", err)
	}
	var cleared int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs WHERE session_id = ''`).Scan(&cleared); err != nil {
		t.Fatalf("count empty sessions: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("empty sessions after a finished pass = %d, want 2", cleared)
	}
}

// The usage plugin carries the executor's CacheReported into request_logs: a
// request whose upstream said nothing about its cache stores NULL reads.
func TestLoggerPluginStoresWhetherCacheUseWasReported(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	plugin := NewLoggerPlugin()
	now := time.Now().UTC()
	plugin.HandleUsage(context.Background(), coreusage.Record{
		APIKey: "sk-plugin-cache", Model: "cache-reported", RequestedAt: now,
		Detail: coreusage.Detail{InputTokens: 1000, TotalTokens: 1000, CacheReadTokens: 400, CachedTokens: 400,
			CacheReadIncludedInInput: true, CacheReported: true},
	})
	plugin.HandleUsage(context.Background(), coreusage.Record{
		APIKey: "sk-plugin-cache", Model: "cache-unknown", RequestedAt: now.Add(time.Second),
		Detail: coreusage.Detail{InputTokens: 1000, TotalTokens: 1000},
	})

	for model, wantRead := range map[string]sql.NullInt64{
		"cache-reported": {Int64: 400, Valid: true},
		"cache-unknown":  {},
	} {
		var read sql.NullInt64
		var prompt int64
		if err := getDB().QueryRow(`SELECT cache_read_tokens, prompt_tokens FROM request_logs WHERE model = ?`, model).Scan(&read, &prompt); err != nil {
			t.Fatalf("read %s: %v", model, err)
		}
		if read != wantRead || prompt != 1000 {
			t.Fatalf("%s cache_read_tokens/prompt_tokens = %+v/%d, want %+v/1000", model, read, prompt, wantRead)
		}
	}
}

func shortenSessionBackfillPauses() func() {
	window, batch, settle := requestLogSessionBackfillWindow, requestLogSessionBackfillBatchPause, requestLogSessionBackfillSettlePause
	// A window smaller than the rows exercises the walk across windows.
	requestLogSessionBackfillWindow = 2
	requestLogSessionBackfillBatchPause = 0
	requestLogSessionBackfillSettlePause = 0
	return func() {
		requestLogSessionBackfillWindow, requestLogSessionBackfillBatchPause, requestLogSessionBackfillSettlePause = window, batch, settle
	}
}

func ptrFloat(v float64) *float64 { return &v }

func sameRate(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) < 1e-9
}

func formatRate(v *float64) string {
	if v == nil {
		return "null"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

package usage

import (
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

var monitorTestZone = time.FixedZone("CST", 8*3600)

// 2026-10-07 is a Wednesday; the 24h window is [10-06 14:00, 10-07 14:00).
var monitorTestNow = time.Date(2026, 10, 7, 13, 47, 30, 0, monitorTestZone)

const monitorOtherTenant = "11111111-2222-3333-4444-555555555555"

type monitorTestEvent struct {
	tenant    string
	apiKey    string
	apiKeyID  string
	endUserID string
	model     string
	channel   string
	failed    bool
	streaming bool
	latency   int64
	ttft      int64
	tokens    TokenStats
	cost      float64
	at        time.Time
	// rollupOnly skips the request_logs row, like a request whose detail
	// already expired.
	rollupOnly bool
}

func initMonitorTestDB(t *testing.T) {
	t.Helper()
	CloseDB()
	if err := InitDB(filepath.Join(t.TempDir(), "monitor.db"), config.RequestLogStorageConfig{StoreContent: false}, monitorTestZone); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	stopRequestLogMaintenance()
	t.Cleanup(CloseDB)
	if _, err := getDB().Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			display_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active'
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := EnsureEndUserQuotaColumns(getDB()); err != nil {
		t.Fatalf("EnsureEndUserQuotaColumns: %v", err)
	}
}

func seedMonitorEvent(t *testing.T, ev monitorTestEvent) {
	t.Helper()
	if ev.tenant == "" {
		ev.tenant = systemTenantID
	}
	tx, err := getDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := projectUsageRollupTx(tx, rollupEvent{
		TenantID: ev.tenant, APIKeyID: ev.apiKeyID, EndUserID: ev.endUserID,
		Model: ev.model, Source: "test", ChannelName: ev.channel,
		Failed: ev.failed, Streaming: ev.streaming,
		LatencyMs: ev.latency, FirstTokenMs: ev.ttft,
		Tokens: ev.tokens, Cost: ev.cost, At: ev.at,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("project rollup: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if ev.rollupOnly {
		return
	}
	failed, streaming := 0, 0
	if ev.failed {
		failed = 1
	}
	if ev.streaming {
		streaming = 1
	}
	if _, err := getDB().Exec(`
		INSERT INTO request_logs (
			tenant_id, timestamp, api_key, api_key_id, model, source, channel_name,
			failed, streaming, latency_ms, first_token_ms,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost
		) VALUES (?, ?, ?, ?, ?, 'test', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, ev.tenant, ev.at.UTC().Format(time.RFC3339Nano), ev.apiKey, ev.apiKeyID, ev.model, ev.channel,
		failed, streaming, ev.latency, ev.ttft,
		ev.tokens.InputTokens, ev.tokens.OutputTokens, ev.tokens.ReasoningTokens, ev.tokens.CachedTokens, ev.tokens.TotalTokens, ev.cost,
	); err != nil {
		t.Fatalf("insert request log: %v", err)
	}
}

// seedMonitorFixture: portal user Alice owns key-a; key-ci stands alone.
func seedMonitorFixture(t *testing.T) {
	t.Helper()
	initMonitorTestDB(t)
	if _, err := getDB().Exec(`INSERT INTO end_users (id, tenant_id, display_name) VALUES (?, ?, ?)`, "eu-alice", systemTenantID, "Alice"); err != nil {
		t.Fatalf("insert end user: %v", err)
	}
	stamp := monitorTestNow.UTC().Format(time.RFC3339)
	for _, row := range []APIKeyRow{
		{ID: "key-a", Key: "sk-alice-XXXXXXXXXXXX1111", Name: "Laptop", EndUserID: "eu-alice", CreatedAt: stamp, UpdatedAt: stamp},
		{ID: "key-ci", Key: "sk-ci-XXXXXXXXXXXXXXXX2222", Name: "CI", CreatedAt: stamp, UpdatedAt: stamp},
	} {
		if err := UpsertAPIKeyForTenant(systemTenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.ID, err)
		}
	}
	day := func(d, hour, minute int) time.Time {
		return time.Date(2026, 10, d, hour, minute, 0, 0, monitorTestZone)
	}
	alice := monitorTestEvent{apiKey: "sk-alice-XXXXXXXXXXXX1111", apiKeyID: "key-a", endUserID: "eu-alice"}
	ci := monitorTestEvent{apiKey: "sk-ci-XXXXXXXXXXXXXXXX2222", apiKeyID: "key-ci"}
	with := func(base monitorTestEvent, fn func(*monitorTestEvent)) monitorTestEvent {
		fn(&base)
		return base
	}
	events := []monitorTestEvent{
		with(alice, func(e *monitorTestEvent) {
			e.model, e.channel, e.at = "gpt-x", "Codex-A", day(7, 12, 10)
			e.latency, e.ttft, e.streaming = 1000, 300, true
			e.tokens = TokenStats{InputTokens: 100, OutputTokens: 50, CachedTokens: 20, TotalTokens: 150}
			e.cost = 0.1
		}),
		with(alice, func(e *monitorTestEvent) {
			e.model, e.channel, e.at = "gpt-x", "Codex-A", day(7, 12, 15)
			e.latency, e.ttft, e.streaming = 3000, 500, true
			e.tokens = TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
		}),
		// Same channel spelled differently: must fold into one row.
		with(alice, func(e *monitorTestEvent) {
			e.model, e.channel, e.at, e.failed = "gpt-x", "codex-a", day(7, 12, 20), true
			e.latency = 9000
		}),
		with(ci, func(e *monitorTestEvent) {
			e.model, e.channel, e.at = "gpt-y", "Claude-B", day(7, 11, 5)
			e.latency = 2000
			e.tokens = TokenStats{InputTokens: 40, OutputTokens: 60, TotalTokens: 100}
			e.cost = 0.4
		}),
		// Previous 24h window.
		with(ci, func(e *monitorTestEvent) {
			e.model, e.channel, e.at = "gpt-y", "Claude-B", day(6, 13, 30)
			e.latency = 2500
		}),
		// Outside both windows.
		with(ci, func(e *monitorTestEvent) {
			e.model, e.channel, e.at = "gpt-y", "Claude-B", day(4, 9, 0)
		}),
		// Another tenant at the same time must never leak in.
		{tenant: monitorOtherTenant, apiKeyID: "key-other", model: "gpt-x", channel: "Codex-A", at: day(7, 12, 30), latency: 100},
	}
	for _, ev := range events {
		seedMonitorEvent(t, ev)
	}
}

func TestQueryMonitorOverviewAggregatesWindow(t *testing.T) {
	seedMonitorFixture(t)
	got, err := QueryMonitorOverview(MonitorFilter{TenantID: systemTenantID}, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}

	cur := got.Summary.Current
	if cur.Requests != 4 || cur.Failed != 1 || cur.Success != 3 || cur.Streaming != 2 {
		t.Fatalf("current = %+v, want 4 requests / 1 failed / 3 success / 2 streaming", cur)
	}
	if math.Abs(cur.SuccessRate-75) > 1e-9 {
		t.Fatalf("success rate = %v, want 75", cur.SuccessRate)
	}
	if math.Abs(cur.Cost-0.5) > 1e-9 || cur.TotalTokens != 265 {
		t.Fatalf("cost/tokens = %v/%d, want 0.5/265", cur.Cost, cur.TotalTokens)
	}
	if want := float64(1000+3000+9000+2000) / 4; math.Abs(cur.LatencyAvgMs-want) > 1e-9 {
		t.Fatalf("latency avg = %v, want %v", cur.LatencyAvgMs, want)
	}
	if math.Abs(cur.FirstTokenAvgMs-400) > 1e-9 {
		t.Fatalf("first token avg = %v, want 400", cur.FirstTokenAvgMs)
	}
	if got.Summary.Previous.Requests != 1 {
		t.Fatalf("previous requests = %d, want 1", got.Summary.Previous.Requests)
	}

	if len(got.Series) != 24 {
		t.Fatalf("series len = %d, want 24", len(got.Series))
	}
	for i := 1; i < len(got.Series); i++ {
		prev, _ := time.Parse(time.RFC3339, got.Series[i-1].Start)
		cur, _ := time.Parse(time.RFC3339, got.Series[i].Start)
		if cur.Sub(prev) != time.Hour {
			t.Fatalf("series not continuous at %d: %s -> %s", i, got.Series[i-1].Start, got.Series[i].Start)
		}
	}
	byStart := map[string]MonitorSeriesPoint{}
	for _, point := range got.Series {
		byStart[point.Start] = point
	}
	if p := byStart["2026-10-07T12:00:00+08:00"]; p.Requests != 3 || p.Failed != 1 {
		t.Fatalf("12:00 bucket = %+v, want 3 requests / 1 failed", p)
	}
	if p := byStart["2026-10-07T11:00:00+08:00"]; p.Requests != 1 {
		t.Fatalf("11:00 bucket = %+v, want 1 request", p)
	}
	if got.Range.StepSeconds != 3600 || got.Range.Start != "2026-10-06T14:00:00+08:00" {
		t.Fatalf("range = %+v", got.Range)
	}

	if len(got.Models.Rows) != 2 || got.Models.Rows[0].Key != "gpt-x" || got.Models.Rows[0].Requests != 3 {
		t.Fatalf("models = %+v, want gpt-x(3) first", got.Models.Rows)
	}
	if trend := got.Models.Rows[0].Trend; len(trend) != 24 || sumInt64(trend) != 3 || sumInt64(got.Models.Rows[0].TrendFailed) != 1 {
		t.Fatalf("gpt-x trend = %v / %v", trend, got.Models.Rows[0].TrendFailed)
	}

	if len(got.Channels.Rows) != 2 {
		t.Fatalf("channels = %+v, want two folded channels", got.Channels.Rows)
	}
	codex := got.Channels.Rows[0]
	if codex.Key != "codex-a" || codex.Label != "Codex-A" || codex.Requests != 3 || codex.Failed != 1 {
		t.Fatalf("codex channel = %+v, want key codex-a label Codex-A 3/1", codex)
	}
	if codex.Provider != "codex" {
		t.Fatalf("codex provider = %q, want codex", codex.Provider)
	}

	consumers := map[string]MonitorBreakdownRow{}
	for _, row := range got.Consumers.Rows {
		consumers[row.Key] = row
	}
	if row := consumers["eu:eu-alice"]; row.Label != "Alice" || row.Kind != "end_user" || row.Requests != 3 {
		t.Fatalf("alice = %+v", row)
	}
	if row := consumers["key:key-ci"]; row.Label != "CI" || row.Kind != "api_key" || row.KeyHint != "sk-…2222" {
		t.Fatalf("ci key = %+v", row)
	}

	// Two successful latencies in the window: 1000, 3000 (Alice) and 2000 (CI).
	if lat := got.Latency.Total; lat.Samples != 3 || lat.P50Ms != 2000 || lat.MaxMs != 3000 {
		t.Fatalf("latency = %+v, want 3 samples, p50 2000, max 3000", lat)
	}
	if ttft := got.Latency.FirstToken; ttft.Samples != 2 || ttft.P50Ms != 400 {
		t.Fatalf("first token = %+v, want 2 samples, p50 400", ttft)
	}
	if got.Latency.CoverageStart != "2026-10-07T11:05:00+08:00" {
		t.Fatalf("coverage start = %q", got.Latency.CoverageStart)
	}

	if len(got.RecentFailures) != 1 || got.RecentFailures[0].Consumer != "Alice" || got.RecentFailures[0].Model != "gpt-x" {
		t.Fatalf("recent failures = %+v", got.RecentFailures)
	}

	var wednesdayNoon MonitorHeatmapCell
	for _, cell := range got.Heatmap.Cells {
		if cell.Weekday == int(time.Wednesday) && cell.Hour == 12 {
			wednesdayNoon = cell
		}
	}
	if wednesdayNoon.Requests != 3 || wednesdayNoon.Failed != 1 || got.Heatmap.Days != 7 {
		t.Fatalf("heatmap wed 12h = %+v (days %d)", wednesdayNoon, got.Heatmap.Days)
	}

	assertMonitorFlowsConserved(t, got.Flows)

	if len(got.Filters.Models) != 2 || len(got.Filters.Channels) != 2 || len(got.Filters.Consumers) != 2 {
		t.Fatalf("filters = %+v", got.Filters)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"sk-alice-XXXXXXXXXXXX1111", "sk-ci-XXXXXXXXXXXXXXXX2222"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("payload leaks raw key %q", secret)
		}
	}
}

func TestQueryMonitorOverviewFiltersNarrowDataButNotOptions(t *testing.T) {
	seedMonitorFixture(t)
	cases := []struct {
		name         string
		filter       MonitorFilter
		wantRequests int64
		wantLatencyN int64
	}{
		{"model", MonitorFilter{Models: []string{"gpt-y"}}, 1, 1},
		{"channel case-insensitive", MonitorFilter{Channels: []string{"CODEX-A"}}, 3, 2},
		{"end user via owned keys", MonitorFilter{EndUserIDs: []string{"eu-alice"}}, 3, 2},
		{"standalone key", MonitorFilter{APIKeyIDs: []string{"key-ci"}}, 1, 1},
		{"user OR key", MonitorFilter{EndUserIDs: []string{"eu-alice"}, APIKeyIDs: []string{"key-ci"}}, 4, 3},
		{"dimensions AND", MonitorFilter{EndUserIDs: []string{"eu-alice"}, Models: []string{"gpt-y"}}, 0, 0},
		// An end user without keys must match nothing, never the whole tenant.
		{"end user without keys", MonitorFilter{EndUserIDs: []string{"eu-nobody"}}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.TenantID = systemTenantID
			got, err := QueryMonitorOverview(tc.filter, MonitorRange24h, monitorTestNow)
			if err != nil {
				t.Fatalf("QueryMonitorOverview: %v", err)
			}
			if got.Summary.Current.Requests != tc.wantRequests {
				t.Fatalf("requests = %d, want %d", got.Summary.Current.Requests, tc.wantRequests)
			}
			if got.Latency.Total.Samples != tc.wantLatencyN {
				t.Fatalf("latency samples = %d, want %d", got.Latency.Total.Samples, tc.wantLatencyN)
			}
			if len(got.Filters.Models) != 2 || len(got.Filters.Consumers) != 2 {
				t.Fatalf("filter options shrank under %s: %+v", tc.name, got.Filters)
			}
		})
	}
}

func TestQueryMonitorOverviewIsTenantScoped(t *testing.T) {
	seedMonitorFixture(t)
	got, err := QueryMonitorOverview(MonitorFilter{TenantID: monitorOtherTenant}, MonitorRange24h, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorOverview: %v", err)
	}
	if got.Summary.Current.Requests != 1 || got.Latency.Total.Samples != 1 || len(got.Consumers.Rows) != 1 {
		t.Fatalf("other tenant sees %+v / %d samples / %d consumers, want only its own request",
			got.Summary.Current, got.Latency.Total.Samples, len(got.Consumers.Rows))
	}
}

func TestQueryMonitorOverviewEmptyHasFullShape(t *testing.T) {
	initMonitorTestDB(t)
	for _, key := range []string{MonitorRange1h, MonitorRange6h, MonitorRangeToday, MonitorRange7d, MonitorRange14d, MonitorRange30d} {
		got, err := QueryMonitorOverview(MonitorFilter{}, key, monitorTestNow)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "null") {
			t.Fatalf("%s payload has null fields: %s", key, raw)
		}
		if len(got.Series) == 0 || len(got.Latency.Total.Histogram) != len(monitorLatencyBoundsMs)+1 {
			t.Fatalf("%s series/histogram not pre-filled", key)
		}
	}
}

func TestQueryMonitorRealtimeUsesLastCompletedMinute(t *testing.T) {
	initMonitorTestDB(t)
	at := func(minute, second int) time.Time {
		return time.Date(2026, 10, 7, 13, minute, second, 0, monitorTestZone)
	}
	for _, ev := range []monitorTestEvent{
		{model: "gpt-x", at: at(46, 5), latency: 1000, tokens: TokenStats{TotalTokens: 10}},
		{model: "gpt-x", at: at(46, 40), latency: 3000, tokens: TokenStats{TotalTokens: 30}},
		{model: "gpt-x", at: at(47, 5), latency: 2000, failed: true},
		{model: "gpt-x", at: at(10, 0), latency: 500},
	} {
		seedMonitorEvent(t, ev)
	}
	got, err := QueryMonitorRealtime(MonitorFilter{}, monitorTestNow)
	if err != nil {
		t.Fatalf("QueryMonitorRealtime: %v", err)
	}
	if len(got.Points) != 60 {
		t.Fatalf("points = %d, want 60", len(got.Points))
	}
	if got.CurrentRPM != 2 || got.CurrentTPM != 40 || got.PeakRPM != 2 {
		t.Fatalf("rpm current/peak = %d/%d tpm %d, want 2/2 tpm 40", got.CurrentRPM, got.PeakRPM, got.CurrentTPM)
	}
	if got.Last5m.Requests != 3 || got.Last5m.Failed != 1 {
		t.Fatalf("last 5m = %+v, want 3 requests / 1 failed", got.Last5m)
	}
	if math.Abs(got.AvgRPM-3.0/59) > 1e-9 {
		t.Fatalf("avg rpm = %v, want completed minutes only", got.AvgRPM)
	}
}

func TestMonitorBinQuantileIsExactForSparseSamples(t *testing.T) {
	samples := []int64{120, 450, 900, 3100, 15000}
	bins := binMonitorSamples(samples)
	cases := map[float64]float64{0.5: 900, 0.9: 3100 + 0.6*(15000-3100), 0: 120, 1: 15000}
	for q, want := range cases {
		if got := monitorBinQuantile(bins, int64(len(samples)), q); math.Abs(got-want) > 1e-6 {
			t.Errorf("q%.2f = %v, want %v", q, got, want)
		}
	}
}

func TestMonitorBinQuantileStaysWithinOneBin(t *testing.T) {
	// Dense samples inside one bin interpolate between its min and max.
	samples := []int64{1000, 1100, 1200, 1400}
	got := monitorBinQuantile(binMonitorSamples(samples), 4, 0.5)
	if got < 1000 || got > 1400 || math.Abs(got-1150) > 1150*0.1 {
		t.Fatalf("median = %v, want ~1150 inside [1000, 1400]", got)
	}
}

func binMonitorSamples(samples []int64) []monitorLatencyBin {
	bins := make([]monitorLatencyBin, len(monitorLatencyBoundsMs)+1)
	for _, v := range samples {
		i := len(monitorLatencyBoundsMs)
		for j, bound := range monitorLatencyBoundsMs {
			if v < bound {
				i = j
				break
			}
		}
		bin := &bins[i]
		if bin.count == 0 || v < bin.min {
			bin.min = v
		}
		bin.max = max(bin.max, v)
		bin.count++
		bin.sum += v
	}
	return bins
}

func assertMonitorFlowsConserved(t *testing.T, graph MonitorFlowGraph) {
	t.Helper()
	if len(graph.Nodes) == 0 || len(graph.Links) == 0 {
		t.Fatalf("flows empty: %+v", graph)
	}
	in, out := map[string]int64{}, map[string]int64{}
	for _, link := range graph.Links {
		out[link.Source] += link.Requests
		in[link.Target] += link.Requests
	}
	for _, node := range graph.Nodes {
		if node.Layer == "model" && in[node.ID] != out[node.ID] {
			t.Fatalf("model node %s in %d != out %d", node.ID, in[node.ID], out[node.ID])
		}
	}
	if in["model:gpt-x"] != 3 || out["consumer:eu:eu-alice"] != 3 || in["channel:codex-a"] != 3 {
		t.Fatalf("flow volumes in=%v out=%v", in, out)
	}
}

func sumInt64(values []int64) int64 {
	var total int64
	for _, v := range values {
		total += v
	}
	return total
}

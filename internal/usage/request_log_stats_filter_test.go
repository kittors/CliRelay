package usage

import (
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestQueryStatsMatchesQueryLogsForMultiChannelSelectors(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertModelPricing("model-target", 1, 2, 0.5); err != nil {
		t.Fatalf("UpsertModelPricing(model-target): %v", err)
	}
	if err := UpsertModelPricing("model-other", 1, 2, 0.5); err != nil {
		t.Fatalf("UpsertModelPricing(model-other): %v", err)
	}

	now := time.Now().UTC()
	InsertLogWithDetailsIdentitySubject("", "key-a", "subject-a", "", "model-target", "source", "Alpha", "auth-a", false, now, 10, 1, TokenStats{
		InputTokens: 80, OutputTokens: 20, CachedTokens: 20, TotalTokens: 100,
		CacheReadTokens: 20, CacheReadIncludedInInput: true, CacheReported: true,
	}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-a", "subject-a", "", "model-other", "source", "Alpha", "auth-a", false, now.Add(time.Second), 10, 1, TokenStats{
		InputTokens: 40, TotalTokens: 40,
	}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-b", "subject-b", "", "model-target", "source", "Beta", "auth-b", true, now.Add(2*time.Second), 10, 1, TokenStats{
		InputTokens: 10, OutputTokens: 250, CachedTokens: 40, TotalTokens: 300,
		CacheReadTokens: 40, CacheReported: true,
	}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-c", "subject-c", "", "model-target", "source", "Gamma", "auth-c", false, now.Add(3*time.Second), 10, 1, TokenStats{
		InputTokens: 10, TotalTokens: 10,
	}, "", "", "")

	tests := []struct {
		name       string
		params     LogQueryParams
		wantDetail bool
	}{
		{
			name:   "two auth subjects",
			params: LogQueryParams{AuthSubjectIDs: []string{"subject-a", "subject-b"}},
		},
		{
			name:   "two channel names",
			params: LogQueryParams{ChannelNames: []string{" alpha ", "BETA"}},
		},
		{
			name: "management subjects with legacy auth indexes",
			params: LogQueryParams{
				AuthSubjectIDs: []string{"subject-a", "subject-b"},
				AuthIndexes:    []string{"auth-a", "auth-b"},
			},
			wantDetail: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := tt.params
			params.Page = 1
			params.Size = 10
			params.Days = 1
			gotDetail := queryStatsRequiresDetailAggregation(normalizeLogQueryParams(params))
			if gotDetail != tt.wantDetail {
				t.Fatalf("queryStatsRequiresDetailAggregation()=%v, want %v for %+v", gotDetail, tt.wantDetail, params)
			}

			logs, err := QueryLogs(params)
			if err != nil {
				t.Fatalf("QueryLogs(): %v", err)
			}
			stats, err := QueryStats(params)
			if err != nil {
				t.Fatalf("QueryStats(): %v", err)
			}
			if logs.Total != 3 || stats.Total != logs.Total {
				t.Fatalf("logs.Total=%d stats.Total=%d, want both 3", logs.Total, stats.Total)
			}
			if stats.TotalTokens != 440 {
				t.Fatalf("stats.TotalTokens=%d, want 440", stats.TotalTokens)
			}
			if math.Abs(stats.SuccessRate-(2.0/3.0*100)) > 1e-9 {
				t.Fatalf("stats.SuccessRate=%v, want %v", stats.SuccessRate, 2.0/3.0*100)
			}
			if stats.TotalCost <= 0 {
				t.Fatalf("stats.TotalCost=%v, want > 0", stats.TotalCost)
			}
			if stats.CacheRate <= 0 {
				t.Fatalf("stats.CacheRate=%v, want > 0", stats.CacheRate)
			}
		})
	}
}

func TestQueryStatsMatchesQueryLogsForChannelAndModel(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Now().UTC()
	InsertLogWithDetailsIdentitySubject("", "key-a", "subject-a", "", "model-target", "source", "Alpha", "auth-a", false, now, 10, 1, TokenStats{TotalTokens: 11}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-a", "subject-a", "", "model-other", "source", "Alpha", "auth-a", false, now.Add(time.Second), 10, 1, TokenStats{TotalTokens: 22}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-b", "subject-b", "", "model-target", "source", "Beta", "auth-b", false, now.Add(2*time.Second), 10, 1, TokenStats{TotalTokens: 33}, "", "", "")

	params := LogQueryParams{
		Page:         1,
		Size:         10,
		Days:         1,
		Models:       []string{"model-target"},
		ChannelNames: []string{"alpha"},
	}
	logs, err := QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs(): %v", err)
	}
	stats, err := QueryStats(params)
	if err != nil {
		t.Fatalf("QueryStats(): %v", err)
	}
	if logs.Total != 1 || stats.Total != logs.Total || stats.TotalTokens != 11 {
		t.Fatalf("logs.Total=%d stats=%+v, want one 11-token row", logs.Total, stats)
	}
}

func TestQueryStatsMatchesQueryLogsForStatuses(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	now := time.Now().UTC()
	InsertLogWithDetailsIdentitySubject("", "key-success", "subject-success", "", "model", "source", "Success", "auth-success", false, now, 10, 1, TokenStats{
		InputTokens: 80, OutputTokens: 20, CachedTokens: 20, TotalTokens: 100,
		CacheReadTokens: 20, CacheReadIncludedInInput: true, CacheReported: true,
	}, "", "", "")
	InsertLogWithDetailsIdentitySubject("", "key-failed", "subject-failed", "", "model", "source", "Failed", "auth-failed", true, now.Add(time.Second), 10, 1, TokenStats{
		InputTokens: 10, OutputTokens: 250, CachedTokens: 40, TotalTokens: 300,
		CacheReadTokens: 40, CacheReported: true,
	}, "", "", "")
	if _, err := getDB().Exec(`UPDATE request_logs SET cost = CASE WHEN failed = 0 THEN 1.25 ELSE 2.5 END`); err != nil {
		t.Fatalf("set detail costs: %v", err)
	}

	tests := []struct {
		status      string
		tokens      int64
		cost        float64
		successRate float64
		cacheRate   float64
	}{
		{status: "success", tokens: 100, cost: 1.25, successRate: 100, cacheRate: 25},
		{status: "failed", tokens: 300, cost: 2.5, successRate: 0, cacheRate: 80},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			params := LogQueryParams{Page: 1, Size: 10, Days: 1, Statuses: []string{tt.status}}
			logs, err := QueryLogs(params)
			if err != nil {
				t.Fatalf("QueryLogs(): %v", err)
			}
			stats, err := QueryStats(params)
			if err != nil {
				t.Fatalf("QueryStats(): %v", err)
			}
			if logs.Total != 1 || stats.Total != logs.Total {
				t.Fatalf("logs.Total=%d stats.Total=%d, want both 1", logs.Total, stats.Total)
			}
			if stats.TotalTokens != tt.tokens || math.Abs(stats.TotalCost-tt.cost) > 1e-12 ||
				math.Abs(stats.SuccessRate-tt.successRate) > 1e-12 || math.Abs(stats.CacheRate-tt.cacheRate) > 1e-12 {
				t.Fatalf("stats=%+v, want tokens=%d cost=%v success=%v cache=%v", stats, tt.tokens, tt.cost, tt.successRate, tt.cacheRate)
			}
		})
	}
}

func TestQueryStatsWithoutFiltersRemainsRollupBacked(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	InsertLogWithDetailsIdentitySubject("", "key-a", "subject-a", "", "model", "source", "Alpha", "auth-a", false, time.Now().UTC(), 10, 1, TokenStats{TotalTokens: 17}, "", "", "")

	params := LogQueryParams{Page: 1, Size: 10, Days: 1}
	logs, err := QueryLogs(params)
	if err != nil {
		t.Fatalf("QueryLogs(): %v", err)
	}
	stats, err := QueryStats(params)
	if err != nil {
		t.Fatalf("QueryStats(): %v", err)
	}
	if logs.Total != 1 || stats.Total != logs.Total || stats.TotalTokens != 17 {
		t.Fatalf("before detail delete logs.Total=%d stats=%+v", logs.Total, stats)
	}
	if _, err := getDB().Exec(`DELETE FROM request_logs`); err != nil {
		t.Fatalf("delete request_logs: %v", err)
	}
	stats, err = QueryStats(params)
	if err != nil {
		t.Fatalf("QueryStats() after detail delete: %v", err)
	}
	if stats.Total != 1 || stats.TotalTokens != 17 {
		t.Fatalf("stats after detail delete=%+v, want rollup total=1 tokens=17", stats)
	}
}

func TestQueryStatsUnknownAPIKeyRemainsFailClosed(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	if err := UpsertAPIKey(APIKeyRow{ID: "known-key-id", Key: "sk-known", Name: "Known"}); err != nil {
		t.Fatalf("UpsertAPIKey(): %v", err)
	}
	now := time.Now().UTC()
	InsertLogWithDetailsIdentity("sk-known", "known-key-id", "Known", "model", "source", "Alpha", "auth-a", false, now, 10, 1, TokenStats{TotalTokens: 19}, "", "", "")
	// This row deliberately has no api_keys registry entry. Even though the raw
	// secret and auth_index match details, stats must keep the identity fail-closed.
	InsertLogWithDetailsIdentity("sk-orphan", "orphan-key-id", "Orphan", "model", "source", "Beta", "auth-orphan", false, now.Add(time.Second), 10, 1, TokenStats{TotalTokens: 23}, "", "", "")

	unfiltered, err := QueryStats(LogQueryParams{Days: 1})
	if err != nil {
		t.Fatalf("QueryStats(unfiltered): %v", err)
	}
	if unfiltered.Total != 2 {
		t.Fatalf("unfiltered stats=%+v, want two tenant rows", unfiltered)
	}
	stats, err := QueryStats(LogQueryParams{
		Days:        1,
		APIKeys:     []string{"sk-orphan"},
		AuthIndexes: []string{"auth-orphan"},
	})
	if err != nil {
		t.Fatalf("QueryStats(unknown key): %v", err)
	}
	if stats != (LogStats{}) {
		t.Fatalf("unknown-key stats=%+v, want all zero without tenant-wide fallback", stats)
	}
}

package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestDashboardQueriesAreTenantScoped(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	db := getDB()
	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	insertDetailAndRollup := func(tenantID string, failed bool, in, out, total int64, cost float64) {
		t.Helper()
		failedInt := 0
		if failed {
			failedInt = 1
		}
		if _, err := db.Exec(`
			INSERT INTO request_logs
			(tenant_id, timestamp, api_key, model, source, channel_name, auth_index, failed,
			 latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens,
			 cached_tokens, total_tokens, cost)
			VALUES (?, ?, '', 'model', 'source', 'channel', 'auth', ?, 1, 1, ?, ?, 0, 0, ?, ?)
		`, tenantID, nowStr, failedInt, in, out, total, cost); err != nil {
			t.Fatalf("insert %s: %v", tenantID, err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := projectUsageRollupTx(tx, rollupEvent{
			TenantID:    tenantID,
			Model:       "model",
			Source:      "source",
			ChannelName: "channel",
			Failed:      failed,
			Tokens:      TokenStats{InputTokens: in, OutputTokens: out, TotalTokens: total},
			Cost:        cost,
			At:          now,
		}); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	insertDetailAndRollup(tenantA, false, 10, 5, 15, 0.1)
	insertDetailAndRollup(tenantB, false, 10, 5, 15, 0.1)
	insertDetailAndRollup(tenantB, true, 20, 10, 30, 0.2)

	kpiA, err := QueryDashboardKPIForTenant(tenantA, 1)
	if err != nil {
		t.Fatal(err)
	}
	if kpiA.TotalRequests != 1 || kpiA.TotalTokens != 15 || kpiA.FailedRequests != 0 {
		t.Fatalf("tenant A KPI leaked: %+v", kpiA)
	}
	kpiB, err := QueryDashboardKPIForTenant(tenantB, 1)
	if err != nil {
		t.Fatal(err)
	}
	if kpiB.TotalRequests != 2 || kpiB.TotalTokens != 45 || kpiB.FailedRequests != 1 {
		t.Fatalf("tenant B KPI = %+v", kpiB)
	}

	// Per-tenant throughput stays isolated.
	trendsA, err := QueryDashboardTrendsForTenant(tenantA, 1)
	if err != nil {
		t.Fatal(err)
	}
	trendsB, err := QueryDashboardTrendsForTenant(tenantB, 1)
	if err != nil {
		t.Fatal(err)
	}
	rpmA := latestThroughputRPM(trendsA.ThroughputSeries)
	rpmB := latestThroughputRPM(trendsB.ThroughputSeries)
	if rpmA != 1 {
		t.Fatalf("tenant A latest RPM = %.0f, want 1", rpmA)
	}
	if rpmB != 2 {
		t.Fatalf("tenant B latest RPM = %.0f, want 2", rpmB)
	}

	// Platform super-admin view aggregates every tenant.
	allSeries, err := QueryDashboardThroughputAcrossTenants()
	if err != nil {
		t.Fatal(err)
	}
	if latestThroughputRPM(allSeries) != 3 {
		t.Fatalf("all-tenant latest RPM = %.0f, want 3", latestThroughputRPM(allSeries))
	}

	// Breakdown returns both tenants
	breakdown, err := QueryDashboardTenantBreakdownThroughput(map[string]string{
		tenantA: "Tenant Alpha",
		tenantB: "Tenant Beta",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(breakdown) < 2 {
		t.Fatalf("breakdown length = %d, want at least 2", len(breakdown))
	}
	var foundA, foundB bool
	for _, item := range breakdown {
		if item.TenantID == tenantA {
			foundA = true
			if item.TenantName != "Tenant Alpha" {
				t.Errorf("tenant A name = %s, want Tenant Alpha", item.TenantName)
			}
			if item.CurrentRPM != 1 {
				t.Errorf("tenant A current RPM = %.0f, want 1", item.CurrentRPM)
			}
		}
		if item.TenantID == tenantB {
			foundB = true
			if item.TenantName != "Tenant Beta" {
				t.Errorf("tenant B name = %s, want Tenant Beta", item.TenantName)
			}
			if item.CurrentRPM != 2 {
				t.Errorf("tenant B current RPM = %.0f, want 2", item.CurrentRPM)
			}
		}
	}
	if !foundA || !foundB {
		t.Fatalf("breakdown missing tenants: foundA=%v, foundB=%v", foundA, foundB)
	}
}

func latestThroughputRPM(series []DashboardThroughputPoint) float64 {
	if len(series) == 0 {
		return 0
	}
	return series[len(series)-1].RPM
}

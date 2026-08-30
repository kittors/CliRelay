package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestGetDashboardSummaryIncludesTrendsAndMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	usage.InsertLog("", "", "gpt-test", "codex", "codex", "auth-1", false, time.Now().UTC(), 100, 20, usage.TokenStats{
		InputTokens:  11,
		OutputTokens: 22,
		TotalTokens:  33,
	}, "", "")

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard-summary?days=7", nil)

	h.GetDashboardSummary(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Days   int `json:"days"`
		Trends struct {
			RequestVolume    []struct{ Label string } `json:"request_volume"`
			SuccessRate      []struct{ Label string } `json:"success_rate"`
			TotalTokens      []struct{ Label string } `json:"total_tokens"`
			FailedRequests   []struct{ Label string } `json:"failed_requests"`
			ThroughputSeries []struct {
				Label string  `json:"label"`
				RPM   float64 `json:"rpm"`
				TPM   float64 `json:"tpm"`
			} `json:"throughput_series"`
		} `json:"trends"`
		Meta struct {
			GeneratedAt string `json:"generated_at"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.Days != 7 {
		t.Fatalf("days = %d, want 7", payload.Days)
	}
	if len(payload.Trends.RequestVolume) != 7 {
		t.Fatalf("request_volume buckets = %d, want 7", len(payload.Trends.RequestVolume))
	}
	if len(payload.Trends.SuccessRate) != 7 {
		t.Fatalf("success_rate buckets = %d, want 7", len(payload.Trends.SuccessRate))
	}
	if len(payload.Trends.TotalTokens) != 7 {
		t.Fatalf("total_tokens buckets = %d, want 7", len(payload.Trends.TotalTokens))
	}
	if len(payload.Trends.FailedRequests) != 7 {
		t.Fatalf("failed_requests buckets = %d, want 7", len(payload.Trends.FailedRequests))
	}
	if len(payload.Trends.ThroughputSeries) != 7 {
		t.Fatalf("throughput_series buckets = %d, want 7", len(payload.Trends.ThroughputSeries))
	}
	if payload.Meta.GeneratedAt == "" {
		t.Fatalf("meta.generated_at is empty")
	}
}

func TestGetDashboardSummaryIncludesTotalCost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := usage.UpsertModelPricing("gpt-cost-test", 1, 2, 0); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}
	usage.InsertLog("", "", "gpt-cost-test", "codex", "codex", "auth-1", false, time.Now().UTC(), 100, 20, usage.TokenStats{
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalTokens:  3000,
	}, "", "")

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard-summary?days=7", nil)

	h.GetDashboardSummary(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		KPI struct {
			TotalCost float64 `json:"total_cost"`
		} `json:"kpi"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.KPI.TotalCost != 0.005 {
		t.Fatalf("kpi.total_cost = %v, want 0.005", payload.KPI.TotalCost)
	}
}

func TestGetDashboardSummaryCountsAPIKeysFromSQLite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	for _, key := range []string{"sk-one", "sk-two", "sk-three"} {
		if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: key, Name: key}); err != nil {
			t.Fatalf("UpsertAPIKey(%q): %v", key, err)
		}
	}

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/dashboard-summary?days=7", nil)

	h.GetDashboardSummary(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		Counts struct {
			APIKeys int `json:"api_keys"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if payload.Counts.APIKeys != 3 {
		t.Fatalf("counts.api_keys = %d, want 3", payload.Counts.APIKeys)
	}
}

func TestGetDashboardSummaryThroughputScopeByPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: false}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	h := &Handler{cfg: &config.Config{}}

	// Ordinary tenant principal → tenant-scoped throughput.
	recTenant := httptest.NewRecorder()
	cTenant, _ := gin.CreateTestContext(recTenant)
	cTenant.Request = httptest.NewRequest(http.MethodGet, "/dashboard-summary?days=1", nil)
	cTenant.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-00000000000a"},
	})
	h.GetDashboardSummary(cTenant)
	if recTenant.Code != http.StatusOK {
		t.Fatalf("tenant status %d body=%s", recTenant.Code, recTenant.Body.String())
	}
	var tenantPayload struct {
		Meta struct {
			ThroughputScope string `json:"throughput_scope"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recTenant.Body.Bytes(), &tenantPayload); err != nil {
		t.Fatalf("unmarshal tenant: %v", err)
	}
	if tenantPayload.Meta.ThroughputScope != "tenant" {
		t.Fatalf("tenant throughput_scope = %q, want tenant", tenantPayload.Meta.ThroughputScope)
	}

	// Platform super-admin → all-tenant throughput scope (aggregation covered in usage tests).
	recAdmin := httptest.NewRecorder()
	cAdmin, _ := gin.CreateTestContext(recAdmin)
	cAdmin.Request = httptest.NewRequest(http.MethodGet, "/dashboard-summary?days=1", nil)
	cAdmin.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-00000000000a"},
	})
	h.GetDashboardSummary(cAdmin)
	if recAdmin.Code != http.StatusOK {
		t.Fatalf("admin status %d body=%s", recAdmin.Code, recAdmin.Body.String())
	}
	var adminPayload struct {
		Trends struct {
			ThroughputSeries []struct{ RPM float64 } `json:"throughput_series"`
			Tenants          []struct {
				TenantID   string `json:"tenant_id"`
				TenantName string `json:"tenant_name"`
			} `json:"tenants"`
		} `json:"trends"`
		Meta struct {
			ThroughputScope string `json:"throughput_scope"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recAdmin.Body.Bytes(), &adminPayload); err != nil {
		t.Fatalf("unmarshal admin: %v", err)
	}
	if adminPayload.Meta.ThroughputScope != "all_tenants" {
		t.Fatalf("admin throughput_scope = %q, want all_tenants", adminPayload.Meta.ThroughputScope)
	}
}

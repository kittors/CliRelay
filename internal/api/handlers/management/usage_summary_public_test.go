package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func setupUsageSummaryTestDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "usage-summary-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func insertTestLog(t *testing.T, apiKey string) {
	t.Helper()
	usage.InsertLog(apiKey, "test", "gpt-4", "test", "chan", "idx", false, time.Now(), 100, 50,
		usage.TokenStats{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		"", "",
	)
}

func TestGetPublicUsageSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupUsageSummaryTestDB(t)

	const (
		keyWithData = "sk-test-key-with-usage"
		keyNoUsage  = "sk-test-key-no-usage"
		keyDisabled = "sk-test-key-disabled"
		keyUnknown  = "sk-test-key-unknown"
		keyLimited  = "sk-test-key-limited"
	)

	// Register keys before InsertLog so rollup rows get stable api_key_id.
	if err := usage.ReplaceAllAPIKeys([]usage.APIKeyRow{
		{ID: "id-with-data", Key: keyWithData, Disabled: false},
		{ID: "id-no-usage", Key: keyNoUsage, Disabled: false},
		{ID: "id-disabled", Key: keyDisabled, Disabled: true},
		{ID: "id-limited", Key: keyLimited, Disabled: false, DailyLimit: 10, TotalQuota: 100, SpendingLimit: 50, DailySpendingLimit: 5},
	}); err != nil {
		t.Fatalf("ReplaceAllAPIKeys: %v", err)
	}

	insertTestLog(t, keyWithData)
	insertTestLog(t, keyWithData)
	insertTestLog(t, keyLimited)

	t.Run("found=true for key with usage today", func(t *testing.T) {
		body := []byte(`{"api_key":"` + keyWithData + `"}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var got struct {
			Found bool   `json:"found"`
			Range string `json:"range"`
			Stats struct {
				TotalCalls int64   `json:"total_calls"`
				QuotaCost  float64 `json:"quota_cost"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !got.Found {
			t.Errorf("found = false, want true")
		}
		if got.Range != "today" {
			t.Errorf("range = %q, want %q", got.Range, "today")
		}
		if got.Stats.TotalCalls != 2 {
			t.Errorf("total_calls = %d, want 2", got.Stats.TotalCalls)
		}
		if got.Stats.QuotaCost < 0 {
			t.Errorf("quota_cost = %f, want >= 0", got.Stats.QuotaCost)
		}
	})

	t.Run("found=true for existing key with no usage today", func(t *testing.T) {
		body := []byte(`{"api_key":"` + keyNoUsage + `"}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var got struct {
			Found bool   `json:"found"`
			Range string `json:"range"`
			Stats struct {
				TotalCalls int64   `json:"total_calls"`
				QuotaCost  float64 `json:"quota_cost"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !got.Found {
			t.Errorf("found = false, want true (key exists in api_keys table)")
		}
		if got.Stats.TotalCalls != 0 {
			t.Errorf("total_calls = %d, want 0", got.Stats.TotalCalls)
		}
	})

	t.Run("found=false for disabled key even with usage logs", func(t *testing.T) {
		body := []byte(`{"api_key":"` + keyDisabled + `"}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var got struct {
			Found bool   `json:"found"`
			Range string `json:"range"`
			Stats struct {
				TotalCalls int64   `json:"total_calls"`
				QuotaCost  float64 `json:"quota_cost"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Found {
			t.Errorf("found = true, want false (key is disabled)")
		}
	})

	t.Run("found=false for unknown key", func(t *testing.T) {
		body := []byte(`{"api_key":"` + keyUnknown + `"}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
		}

		var got struct {
			Found bool   `json:"found"`
			Range string `json:"range"`
			Stats struct {
				TotalCalls int64   `json:"total_calls"`
				QuotaCost  float64 `json:"quota_cost"`
			} `json:"stats"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Found {
			t.Errorf("found = true, want false (unknown key)")
		}
		// Fail-closed: unknown keys must not return tenant-wide aggregates.
		if got.Stats.TotalCalls != 0 || got.Stats.QuotaCost != 0 {
			t.Errorf("unknown key stats = %+v, want zeros", got.Stats)
		}
	})

	t.Run("returns 400 for empty api_key", func(t *testing.T) {
		body := []byte(`{}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 400 for missing body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", nil)

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 400 when api_key is passed as query param with empty body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary?api_key="+keyWithData, nil)

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (query param api_key must be rejected); body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("includes limits only when configured", func(t *testing.T) {
		body := []byte(`{"api_key":"` + keyLimited + `"}`)
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		h := NewHandler(&config.Config{}, "", nil)
		h.GetPublicUsageSummary(ctx)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var got struct {
			Limits *struct {
				DailyLimit         *int     `json:"daily-limit"`
				DailyUsed          *int64   `json:"daily-used"`
				TotalQuota         *int     `json:"total-quota"`
				TotalUsed          *int64   `json:"total-used"`
				SpendingLimit      *float64 `json:"spending-limit"`
				SpendingUsed       *float64 `json:"spending-used"`
				DailySpendingLimit *float64 `json:"daily-spending-limit"`
				DailySpendingUsed  *float64 `json:"daily-spending-used"`
			} `json:"limits"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Limits == nil {
			t.Fatal("limits = nil, want configured limits")
		}
		if got.Limits.DailyLimit == nil || *got.Limits.DailyLimit != 10 {
			t.Fatalf("daily-limit = %v, want 10", got.Limits.DailyLimit)
		}
		if got.Limits.DailyUsed == nil || *got.Limits.DailyUsed != 1 {
			t.Fatalf("daily-used = %v, want 1", got.Limits.DailyUsed)
		}
		if got.Limits.TotalQuota == nil || *got.Limits.TotalQuota != 100 {
			t.Fatalf("total-quota = %v, want 100", got.Limits.TotalQuota)
		}
		if got.Limits.SpendingLimit == nil || *got.Limits.SpendingLimit != 50 {
			t.Fatalf("spending-limit = %v, want 50", got.Limits.SpendingLimit)
		}
		if got.Limits.DailySpendingLimit == nil || *got.Limits.DailySpendingLimit != 5 {
			t.Fatalf("daily-spending-limit = %v, want 5", got.Limits.DailySpendingLimit)
		}

		// unlimited key omits limits
		body = []byte(`{"api_key":"` + keyWithData + `"}`)
		rec = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.GetPublicUsageSummary(ctx)
		var unlimited struct {
			Limits *struct {
				DailyLimit *int `json:"daily-limit"`
			} `json:"limits"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &unlimited); err != nil {
			t.Fatalf("unmarshal unlimited: %v", err)
		}
		if unlimited.Limits != nil {
			t.Fatalf("limits = %+v, want nil for unlimited key", unlimited.Limits)
		}
	})
}

// Regression: multi-tenant keys must not be looked up under the system tenant.
// CC Switch polls this endpoint with the raw API key only; without ResolveAPIKeyTenant
// the card always shows 0 calls / $0 even when request_logs has real usage.
func TestGetPublicUsageSummary_ResolvesBusinessTenant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupUsageSummaryTestDB(t)

	const (
		tenantID = "00000000-0000-0000-0000-0000000000aa"
		apiKey   = "sk-business-tenant-usage"
	)

	if err := usage.UpsertAPIKeyForTenant(tenantID, usage.APIKeyRow{
		Key:      apiKey,
		Name:     "business-user",
		Disabled: false,
	}); err != nil {
		t.Fatalf("UpsertAPIKeyForTenant: %v", err)
	}

	// InsertLog derives tenant from the API key row, so this lands in the business tenant.
	usage.InsertLog(apiKey, "business-user", "gpt-5.4", "test", "chan", "idx", false, time.Now(), 100, 50,
		usage.TokenStats{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
		"", "",
	)
	usage.InsertLog(apiKey, "business-user", "gpt-5.4", "test", "chan", "idx", false, time.Now(), 100, 50,
		usage.TokenStats{InputTokens: 5, OutputTokens: 5, TotalTokens: 10},
		"", "",
	)

	// Regression guard: public secret lookup discovers the owning tenant globally,
	// while the explicit tenant-scoped lookup still resolves the same row.
	if row := usage.GetAPIKey(apiKey); row == nil || row.TenantID != tenantID {
		t.Fatalf("GetAPIKey = %#v, want business tenant key", row)
	}
	if row := usage.GetAPIKeyForTenant(tenantID, apiKey); row == nil {
		t.Fatalf("business tenant key missing")
	}

	body := []byte(`{"api_key":"` + apiKey + `"}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h := NewHandler(&config.Config{}, "", nil)
	h.GetPublicUsageSummary(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got struct {
		Found bool   `json:"found"`
		Range string `json:"range"`
		Stats struct {
			TotalCalls int64   `json:"total_calls"`
			QuotaCost  float64 `json:"quota_cost"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Found {
		t.Errorf("found = false, want true for business-tenant key")
	}
	if got.Stats.TotalCalls != 2 {
		t.Errorf("total_calls = %d, want 2", got.Stats.TotalCalls)
	}
}

func TestGetPublicUsageSummary_AggregatesOwnedBusinessTenantKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupUsageSummaryTestDB(t)

	tenantID := "00000000-0000-0000-0000-0000000000ab"
	endUserID := "00000000-0000-0000-0000-0000000000bc"
	keyA := "sk-business-owned-a"
	keyB := "sk-business-owned-b"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := usage.RuntimeDB().Exec(`CREATE TABLE IF NOT EXISTS end_users (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
		permission_profile_id TEXT NOT NULL DEFAULT '', daily_limit INTEGER NOT NULL DEFAULT 0, total_quota INTEGER NOT NULL DEFAULT 0,
		spending_limit REAL NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
		five_hour_spending_limit REAL NOT NULL DEFAULT 0, weekly_spending_limit REAL NOT NULL DEFAULT 0, monthly_spending_limit REAL NOT NULL DEFAULT 0,
		concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0,
		allowed_models TEXT NOT NULL DEFAULT '[]', allowed_channels TEXT NOT NULL DEFAULT '[]', allowed_channel_groups TEXT NOT NULL DEFAULT '[]', system_prompt TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if _, err := usage.RuntimeDB().Exec(`INSERT INTO end_users(id,tenant_id,display_name,daily_spending_limit) VALUES(?,?,?,300)`, endUserID, tenantID, "Business"); err != nil {
		t.Fatalf("insert end user: %v", err)
	}
	for _, row := range []usage.APIKeyRow{
		{ID: "00000000-0000-0000-0000-0000000000a1", Key: keyA, Name: "Laptop", EndUserID: endUserID, DailySpendingLimit: 100, PeriodSpendingLimits: quota.PeriodSpendingLimits{Day: 100}, CreatedAt: now, UpdatedAt: now},
		{ID: "00000000-0000-0000-0000-0000000000b1", Key: keyB, Name: "Automation", EndUserID: endUserID, DailySpendingLimit: 100, PeriodSpendingLimits: quota.PeriodSpendingLimits{Day: 100}, CreatedAt: now, UpdatedAt: now},
	} {
		if err := usage.UpsertAPIKeyForTenant(tenantID, row); err != nil {
			t.Fatalf("UpsertAPIKeyForTenant(%s): %v", row.Key, err)
		}
		insertTestLog(t, row.Key)
	}

	body := []byte(`{"api_key":"` + keyB + `"}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/public/usage/summary", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h := NewHandler(&config.Config{}, "", nil)
	h.GetPublicUsageSummary(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got struct {
		Found bool `json:"found"`
		Stats struct {
			TotalCalls int64 `json:"total_calls"`
		} `json:"stats"`
		QuotaScopes []struct {
			Scope          string                 `json:"scope"`
			PeriodSpending []quota.PeriodSpending `json:"period-spending"`
		} `json:"quota-scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Raw secret summary is key-scoped for call stats (not whole account).
	if !got.Found || got.Stats.TotalCalls != 1 {
		t.Fatalf("summary = %+v, want found and one key call", got)
	}
	// Raw secret no longer promotes to EndUserID, so only key-scope quota cards appear.
	if len(got.QuotaScopes) != 1 || got.QuotaScopes[0].Scope != "key" {
		t.Fatalf("quota scopes = %+v, want key only", got.QuotaScopes)
	}
}

package configaccess

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quota"
)

func TestProviderEmitsDailySpendingLimitMetadata(t *testing.T) {
	cfg := &config.SDKConfig{
		APIKeyEntries: []config.APIKeyEntry{{
			Key:                "sk-daily-cost",
			DailySpendingLimit: 4.5,
		}},
	}
	p := newProvider("test", buildKeyConfigMap(cfg))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-daily-cost")
	res, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("Authenticate() error = %v", authErr)
	}
	if got := res.Metadata["daily-spending-limit"]; got != "4.500000" {
		t.Fatalf("daily-spending-limit metadata = %q, want 4.500000", got)
	}
}

func TestProviderEmitsSeparateAccountAndKeyPeriodMetadata(t *testing.T) {
	p := newProvider("test", map[string]keyConfig{
		"sk-owned": {
			tenantID: "tenant-a", apiKeyID: "key-a", endUserID: "user-a",
			accountPeriodSpendingLimits: quota.PeriodSpendingLimits{FiveHour: 100, Day: 300, Week: 800, Month: 4000},
			keyPeriodSpendingLimits:     quota.PeriodSpendingLimits{FiveHour: 50, Day: 100},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-owned")
	res, authErr := p.Authenticate(context.Background(), req)
	if authErr != nil {
		t.Fatalf("Authenticate: %v", authErr)
	}
	checks := map[string]string{
		"tenant-id": "tenant-a", "api-key-id": "key-a", "end-user-id": "user-a",
		"account-period-spending-limit-5h":    "100.000000",
		"account-period-spending-limit-day":   "300.000000",
		"account-period-spending-limit-week":  "800.000000",
		"account-period-spending-limit-month": "4000.000000",
		"key-period-spending-limit-5h":        "50.000000",
		"key-period-spending-limit-day":       "100.000000",
	}
	for key, want := range checks {
		if got := res.Metadata[key]; got != want {
			t.Fatalf("metadata[%s]=%q, want %q", key, got, want)
		}
	}
}

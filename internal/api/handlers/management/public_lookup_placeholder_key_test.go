package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// setupPlaceholderLookupDB stores the example key as an ordinary enabled row with
// recorded traffic, the state an instance that imported it is left in.
func setupPlaceholderLookupDB(t *testing.T) int64 {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	for _, row := range []usage.APIKeyRow{
		{Key: "your-api-key-1", Name: "api-key-1"},
		{Key: "sk-lookup-real", Name: "real"},
	} {
		if err := usage.UpsertAPIKey(row); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", row.Name, err)
		}
	}
	for _, key := range []string{"your-api-key-1", "sk-lookup-real"} {
		usage.InsertLogWithDetails(
			key, "", "gpt-test", "codex", "Codex", "auth-1",
			false, time.Now().UTC(), 100, 10,
			usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			`{"messages":[{"role":"user","content":"private prompt"}]}`, `{"choices":[]}`, "",
		)
	}
	if err := usage.ReplaceAllCcSwitchImportConfigs([]usage.CcSwitchImportConfigRow{{
		ID: "codex-1", ClientType: "codex", ProviderName: "Codex", DefaultModel: "gpt-test", EndpointPath: "/openai",
	}}); err != nil {
		t.Fatalf("ReplaceAllCcSwitchImportConfigs: %v", err)
	}

	result, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1, APIKeys: []string{"your-api-key-1"}})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("QueryLogs(example key) = %d items, err %v; want 1", len(result.Items), err)
	}
	return result.Items[0].ID
}

func callPublicLookup(t *testing.T, handler gin.HandlerFunc, apiKey string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = params
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/public/lookup", bytes.NewReader([]byte(`{"api_key":"`+apiKey+`"}`)))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return rec
}

// The public lookup treats "knows the key" as proof of ownership. The example keys
// are published with the repository, so answering for them would hand anyone the
// usage, and with stored content the prompts, recorded under them.
func TestPublicUsageLookupsRejectPlaceholderAPIKeys(t *testing.T) {
	logID := setupPlaceholderLookupDB(t)
	h := NewHandler(&config.Config{}, "", nil)
	logs := h.UsageLogs()
	idParam := gin.Params{{Key: "id", Value: strconv.FormatInt(logID, 10)}}

	for name, handler := range map[string]gin.HandlerFunc{
		"usage":       h.GetPublicUsageByAPIKey,
		"summary":     h.GetPublicUsageSummary,
		"logs":        logs.GetPublicUsageLogs,
		"chart-data":  logs.GetPublicUsageChartData,
		"log-content": logs.GetPublicLogContent,
	} {
		rec := callPublicLookup(t, handler, "your-api-key-1", idParam)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Invalid API key") {
			t.Errorf("%s with the example key: status = %d body=%s, want 401 Invalid API key", name, rec.Code, rec.Body.String())
		}
	}

	rec := callPublicLookup(t, logs.GetPublicUsageLogs, "sk-lookup-real", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs with a real key: status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Items) != 1 {
		t.Fatalf("logs with a real key: %d items (err %v), want its own entry; body=%s", len(payload.Items), err, rec.Body.String())
	}
}

// CC Switch presets answer an unknown or disabled key with an empty list; an
// example key is neither a credential nor evidence of access, so it gets the same.
func TestPublicCcSwitchImportConfigsTreatPlaceholderKeysAsUnknown(t *testing.T) {
	setupPlaceholderLookupDB(t)
	h := NewHandler(&config.Config{}, "", nil)

	var got struct {
		Items []usage.CcSwitchImportConfigRow `json:"items"`
		Found bool                            `json:"found"`
	}
	rec := callPublicLookup(t, h.GetPublicCcSwitchImportConfigs, "your-api-key-1", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || got.Found || len(got.Items) != 0 {
		t.Fatalf("example key: status = %d found=%v items=%d, want 200 with nothing found", rec.Code, got.Found, len(got.Items))
	}

	rec = callPublicLookup(t, h.GetPublicCcSwitchImportConfigs, "sk-lookup-real", nil)
	got.Items, got.Found = nil, false
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || !got.Found || len(got.Items) != 1 {
		t.Fatalf("real key: status = %d found=%v items=%d, want its preset", rec.Code, got.Found, len(got.Items))
	}
}

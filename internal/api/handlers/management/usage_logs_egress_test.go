package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetUsageLogEgressReturnsProxyProbeDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "proxy-auth",
		FileName: "proxy-auth.json",
		Provider: "codex",
		Label:    "Proxy Auth",
		ProxyID:  "premium-egress",
		ProxyURL: "http://legacy-proxy.local:8080",
		Metadata: map[string]any{
			"label": "Proxy Auth",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	details := `{"egress":{"route_kind":"proxy","proxy_source":"proxy_id","proxy_id":"premium-egress","proxy_name":"Premium egress","proxy_url_host":"http://pool-proxy.local:8080"}}`
	usage.InsertLogWithDetails(
		"sk-test", "Primary", "gpt-test", "codex", "Codex", auth.Index,
		false, time.Now().UTC(), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		`{"messages":[]}`, `{"choices":[]}`, details,
	)
	rows, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(rows.Items) != 1 {
		t.Fatalf("expected one log row, got %d", len(rows.Items))
	}

	savedProbe := usageLogEgressProbeFn
	usageLogEgressProbeFn = func(_ context.Context, proxyURL string, _ *config.SDKConfig) (string, error) {
		switch proxyURL {
		case "":
			return "198.51.100.10", nil
		case "http://pool-proxy.local:8080":
			return "203.0.113.50", nil
		default:
			return "", fmt.Errorf("unexpected proxy url %q", proxyURL)
		}
	}
	t.Cleanup(func() {
		usageLogEgressProbeFn = savedProbe
	})

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "premium-egress", Name: "Premium egress", URL: "http://pool-proxy.local:8080", Enabled: true},
			},
		},
		authManager: manager,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(rows.Items[0].ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs/1/egress", nil)

	h.GetUsageLogEgress(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload struct {
		RouteKind       string `json:"route_kind"`
		ProxyID         string `json:"proxy_id"`
		ProxyName       string `json:"proxy_name"`
		ProxyURLHost    string `json:"proxy_url_host"`
		EffectiveIP     string `json:"effective_ip"`
		ServerIP        string `json:"server_ip"`
		MatchesServerIP *bool  `json:"matches_server_ip"`
		UsingProxy      bool   `json:"using_proxy"`
		Error           string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.RouteKind != "proxy" {
		t.Fatalf("route_kind = %q, want proxy", payload.RouteKind)
	}
	if !payload.UsingProxy {
		t.Fatal("using_proxy = false, want true")
	}
	if payload.ProxyID != "premium-egress" {
		t.Fatalf("proxy_id = %q, want premium-egress", payload.ProxyID)
	}
	if payload.ProxyName != "Premium egress" {
		t.Fatalf("proxy_name = %q, want Premium egress", payload.ProxyName)
	}
	if payload.ProxyURLHost != "http://pool-proxy.local:8080" {
		t.Fatalf("proxy_url_host = %q, want masked proxy host", payload.ProxyURLHost)
	}
	if payload.ServerIP != "198.51.100.10" {
		t.Fatalf("server_ip = %q, want direct server IP", payload.ServerIP)
	}
	if payload.EffectiveIP != "203.0.113.50" {
		t.Fatalf("effective_ip = %q, want proxy egress IP", payload.EffectiveIP)
	}
	if payload.MatchesServerIP == nil || *payload.MatchesServerIP {
		t.Fatalf("matches_server_ip = %#v, want false", payload.MatchesServerIP)
	}
	if payload.Error != "" {
		t.Fatalf("error = %q, want empty", payload.Error)
	}
}

func TestGetUsageLogEgressHandlesHistoricalLogsWithoutMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	usage.InsertLogWithDetails(
		"sk-test", "Primary", "gpt-test", "codex", "Codex", "auth-1",
		false, time.Now().UTC(), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		`{"messages":[]}`, `{"choices":[]}`, `{"client":{"ip":"203.0.113.8"}}`,
	)
	rows, err := usage.QueryLogs(usage.LogQueryParams{Page: 1, Size: 10, Days: 1})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(rows.Items) != 1 {
		t.Fatalf("expected one log row, got %d", len(rows.Items))
	}

	probeCalled := false
	savedProbe := usageLogEgressProbeFn
	usageLogEgressProbeFn = func(_ context.Context, _ string, _ *config.SDKConfig) (string, error) {
		probeCalled = true
		return "", fmt.Errorf("unexpected probe")
	}
	t.Cleanup(func() {
		usageLogEgressProbeFn = savedProbe
	})

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(rows.Items[0].ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs/1/egress", nil)

	h.GetUsageLogEgress(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d, body=%s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if probeCalled {
		t.Fatal("egress probe should not run when request has no stored egress metadata")
	}
	var payload struct {
		Error      string `json:"error"`
		UsingProxy bool   `json:"using_proxy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Error == "" {
		t.Fatal("expected a missing-metadata error")
	}
	if payload.UsingProxy {
		t.Fatal("using_proxy = true, want false")
	}
}

func TestExtractUsageLogProbeIPParsesCloudflareTrace(t *testing.T) {
	body := []byte("fl=1230f106\nh=chatgpt.com\nip=66.227.101.209\nts=1781158999.000\n")
	if got := extractUsageLogProbeIP(body); got != "66.227.101.209" {
		t.Fatalf("extractUsageLogProbeIP(trace) = %q, want %q", got, "66.227.101.209")
	}
}

func TestProbeUsageLogEgressIPFallsBackToPlainIPProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/trace":
			_, _ = w.Write([]byte("fl=1230f106\nh=chatgpt.com\nwarp=off\n"))
		case "/plain":
			_, _ = w.Write([]byte("203.0.113.50\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	savedProbeURLs := usageLogEgressProbeURLs
	usageLogEgressProbeURLs = []string{
		server.URL + "/trace",
		server.URL + "/plain",
	}
	t.Cleanup(func() {
		usageLogEgressProbeURLs = savedProbeURLs
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := probeUsageLogEgressIP(ctx, "", nil)
	if err != nil {
		t.Fatalf("probeUsageLogEgressIP: %v", err)
	}
	if got != "203.0.113.50" {
		t.Fatalf("probeUsageLogEgressIP = %q, want %q", got, "203.0.113.50")
	}
}

func TestGetUsageLogEgressCannotReadAnotherTenantLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	const apiKeyA = "sk-tenant-a-egress"
	if err := usage.UpsertAPIKeyForTenant(tenantA, usage.APIKeyRow{Key: apiKeyA}); err != nil {
		t.Fatalf("UpsertAPIKeyForTenant: %v", err)
	}
	usage.InsertLogWithDetails(
		apiKeyA, "Tenant A", "gpt-test", "codex", "Codex", "auth-a",
		false, time.Now().UTC(), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		`{"messages":[]}`, `{"choices":[]}`, `{"egress":{"route_kind":"direct"}}`,
	)
	rows, err := usage.QueryLogs(usage.LogQueryParams{TenantID: tenantA, Page: 1, Size: 10, Days: 1})
	if err != nil || len(rows.Items) != 1 {
		t.Fatalf("QueryLogsForTenant: items=%d err=%v", len(rows.Items), err)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantB}})
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(rows.Items[0].ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs/1/egress", nil)

	(&Handler{cfg: &config.Config{}}).GetUsageLogEgress(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestGetUsageLogEgressRechecksSharedCredentialThroughThePlatformPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	const grantee = "00000000-0000-0000-0000-00000000000a"
	const apiKey = "sk-grantee-egress"
	if err := usage.UpsertAPIKeyForTenant(grantee, usage.APIKeyRow{Key: apiKey}); err != nil {
		t.Fatalf("UpsertAPIKeyForTenant: %v", err)
	}
	systemPool := []config.ProxyPoolEntry{{ID: "hk-1", Name: "Platform HK", URL: "http://platform-proxy.local:8080", Enabled: true}}
	if err := usage.ReplaceProxyPool(systemPool); err != nil {
		t.Fatal(err)
	}
	// The grantee has an unrelated entry under the same ID. The request left
	// through the platform's hk-1, where the shared credential runs.
	if err := usage.ReplaceProxyPoolForTenant(grantee, []config.ProxyPoolEntry{{ID: "hk-1", URL: "http://grantee-proxy.local:8080", Enabled: true}}); err != nil {
		t.Fatal(err)
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	shared, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-platform.json", TenantID: identity.SystemTenantID, Provider: "codex", ProxyID: "hk-1", Status: coreauth.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSharedAuthGrants(map[string][]string{shared.ID: {grantee}})

	usage.InsertLogWithDetails(
		apiKey, "Grantee", "gpt-test", "codex", "Codex", shared.Index,
		false, time.Now().UTC(), 100, 10,
		usage.TokenStats{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		`{"messages":[]}`, `{"choices":[]}`,
		`{"egress":{"route_kind":"proxy","proxy_source":"proxy_id","proxy_id":"hk-1","proxy_name":"Platform HK","proxy_url_host":"http://platform-proxy.local:8080"}}`,
	)
	rows, err := usage.QueryLogs(usage.LogQueryParams{TenantID: grantee, Page: 1, Size: 10, Days: 1})
	if err != nil || len(rows.Items) != 1 {
		t.Fatalf("QueryLogs: items=%d err=%v", len(rows.Items), err)
	}

	savedProbe := usageLogEgressProbeFn
	var probed []string
	usageLogEgressProbeFn = func(_ context.Context, proxyURL string, _ *config.SDKConfig) (string, error) {
		probed = append(probed, proxyURL)
		return "203.0.113.7", nil
	}
	t.Cleanup(func() { usageLogEgressProbeFn = savedProbe })

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: grantee}})
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(rows.Items[0].ID, 10)}}
	c.Request = httptest.NewRequest(http.MethodGet, "/usage/logs/1/egress", nil)
	(&Handler{cfg: &config.Config{ProxyPool: systemPool}, authManager: manager}).GetUsageLogEgress(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(probed) != 2 || probed[1] != "http://platform-proxy.local:8080" {
		t.Fatalf("probed %v, want the server and then the platform's hk-1", probed)
	}
}

package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestProviderSettingsAreTenantScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	h := &Handler{cfg: &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "system-key"}}}}
	keys := h.ProviderKeys()
	request := func(tenantID, method string, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
		c.Request = httptest.NewRequest(method, "/v0/management/gemini-api-key", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler(c)
		return rec
	}

	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	if rec := request(tenantA, http.MethodPut, `[{"api-key":"tenant-a-key"}]`, keys.PutGeminiKeys); rec.Code != http.StatusOK {
		t.Fatalf("tenant A PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if h.cfg.GeminiKey[0].APIKey != "system-key" {
		t.Fatalf("system provider config mutated: %+v", h.cfg.GeminiKey)
	}
	if rec := request(tenantB, http.MethodGet, "", keys.GetGeminiKeys); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "tenant-a-key") || strings.Contains(rec.Body.String(), "system-key") {
		t.Fatalf("tenant B GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request(tenantA, http.MethodGet, "", keys.GetGeminiKeys); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "tenant-a-key") {
		t.Fatalf("tenant A GET status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderSettingsRegisterTenantRuntimeCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{
		cfg:         &config.Config{GeminiKey: []config.GeminiKey{{APIKey: "system-key"}}},
		authManager: manager,
	}
	keys := h.ProviderKeys()
	request := func(tenantID, apiKey string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenantID}})
		c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/gemini-api-key", bytes.NewBufferString(`[{"api-key":"`+apiKey+`"}]`))
		c.Request.Header.Set("Content-Type", "application/json")
		keys.PutGeminiKeys(c)
		return rec
	}

	const tenantA = "00000000-0000-0000-0000-00000000000a"
	const tenantB = "00000000-0000-0000-0000-00000000000b"
	for _, tenantID := range []string{tenantA, tenantB} {
		if rec := request(tenantID, "shared-key"); rec.Code != http.StatusOK {
			t.Fatalf("tenant %s PUT status=%d body=%s", tenantID, rec.Code, rec.Body.String())
		}
	}

	authA := manager.ListForTenant(tenantA)
	authB := manager.ListForTenant(tenantB)
	if len(authA) != 1 || len(authB) != 1 {
		t.Fatalf("tenant auth counts = %d/%d, want 1/1", len(authA), len(authB))
	}
	if authA[0].ID == authB[0].ID || !strings.HasPrefix(authA[0].ID, tenantA+"/") || !strings.HasPrefix(authB[0].ID, tenantB+"/") {
		t.Fatalf("tenant auth IDs are not namespaced: %q / %q", authA[0].ID, authB[0].ID)
	}
	if authA[0].TenantID != tenantA || authB[0].TenantID != tenantB {
		t.Fatalf("tenant auth scopes = %q / %q", authA[0].TenantID, authB[0].TenantID)
	}
	execA, okA := manager.ExecutorForTenant(tenantA, "gemini")
	execB, okB := manager.ExecutorForTenant(tenantB, "gemini")
	if !okA || !okB || execA == execB {
		t.Fatalf("tenant executors = %#v/%v %#v/%v, want isolated executors", execA, okA, execB, okB)
	}
	if _, ok := manager.Executor("gemini"); ok {
		t.Fatal("tenant provider update unexpectedly registered a system executor")
	}
}

func TestGetConfigReturnsOnlyTenantRuntimeView(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	h := &Handler{cfg: &config.Config{
		SDKConfig: config.SDKConfig{RequestLog: true},
		GeminiKey: []config.GeminiKey{{APIKey: "system-secret"}},
		Debug:     true,
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   false,
		EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-00000000000a"},
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	h.GetConfig(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "system-secret") || strings.Contains(body, `"debug":true`) {
		t.Fatalf("tenant config leaked system settings: %s", body)
	}
	// Non-platform tenants still must not receive host runtime toggles via /config
	// (they reach dedicated endpoints only when authorized).
	if strings.Contains(body, `"request-log":true`) {
		t.Fatalf("non-platform tenant config leaked process-global request-log: %s", body)
	}
}

func TestGetConfigIncludesProcessGlobalTogglesForPlatformAdminInTenantContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	h := &Handler{cfg: &config.Config{
		SDKConfig: config.SDKConfig{
			RequestLog:       true,
			ForceModelPrefix: true,
			ProxyURL:         "http://host-proxy.example:7890",
		},
		GeminiKey:              []config.GeminiKey{{APIKey: "system-secret"}},
		Debug:                  true,
		LoggingToFile:          true,
		UsageStatisticsEnabled: true,
		WebsocketAuth:          true,
		QuotaExceeded:          config.QuotaExceeded{SwitchProject: true, SwitchPreviewModel: true},
	}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(managementPrincipalKey, identity.Principal{
		PlatformAdmin:   true,
		EffectiveTenant: identity.Tenant{ID: "00000000-0000-0000-0000-00000000000a"},
		HomeTenant:      identity.Tenant{ID: identity.SystemTenantID},
	})
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/config", nil)
	h.GetConfig(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "system-secret") {
		t.Fatalf("platform-admin tenant view leaked provider secrets: %s", body)
	}
	for _, want := range []string{
		`"request-log":true`,
		`"logging-to-file":true`,
		`"debug":true`,
		`"usage-statistics-enabled":true`,
		`"ws-auth":true`,
		`"force-model-prefix":true`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("platform-admin tenant view missing %s: %s", want, body)
		}
	}
}

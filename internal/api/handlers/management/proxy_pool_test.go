package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestGetProxyPoolIncludesMaskedURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "hk", Name: "HK", URL: "socks5://user:pass@127.0.0.1:1080/path", Enabled: true},
		},
	}, "", nil)
	defer h.Close()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetProxyPool(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Items []struct {
			ID        string `json:"id"`
			URL       string `json:"url"`
			MaskedURL string `json:"masked_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items length = %d, want 1", len(body.Items))
	}
	if body.Items[0].URL != "socks5://user:pass@127.0.0.1:1080/path" {
		t.Fatalf("url = %q", body.Items[0].URL)
	}
	if body.Items[0].MaskedURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("masked_url = %q, want socks5://127.0.0.1:1080", body.Items[0].MaskedURL)
	}
}

func TestPutProxyPoolNormalizesAndPersists(t *testing.T) {
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(&config.Config{}, configPath, nil)
	defer h.Close()

	payload := []byte(`{"items":[{"id":" HK ","name":" HK Proxy ","url":" http://127.0.0.1:7890 ","enabled":true},{"id":"bad","name":"bad","url":"ftp://bad","enabled":true}]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/proxy-pool", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyPool(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(h.cfg.ProxyPool) != 1 || h.cfg.ProxyPool[0].ID != "hk" {
		t.Fatalf("proxy pool = %#v", h.cfg.ProxyPool)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "proxy-pool:") || !strings.Contains(string(data), "id: hk") {
		t.Fatalf("persisted config missing proxy pool:\n%s", string(data))
	}
}

func TestPatchProxyPoolEntryUpdatesConfigWithoutAppending(t *testing.T) {
	gin.SetMode(gin.TestMode)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initialConfig := "proxy-pool:\n  - id: hk\n    name: HK Proxy\n    url: http://127.0.0.1:7890\n    enabled: true\n"
	if err := os.WriteFile(configPath, []byte(initialConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "hk", Name: "HK Proxy", URL: "http://127.0.0.1:7890", Enabled: true},
		},
	}, configPath, nil)
	defer h.Close()

	payload := []byte(`{"name":"Updated HK Proxy","url":"http://127.0.0.1:7891","enabled":false,"description":"rotated"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "hk"}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/proxy-pool/hk", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchProxyPoolEntry(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(h.cfg.ProxyPool) != 1 {
		t.Fatalf("proxy pool length = %d, want 1: %#v", len(h.cfg.ProxyPool), h.cfg.ProxyPool)
	}
	if h.cfg.ProxyPool[0].ID != "hk" || h.cfg.ProxyPool[0].Name != "Updated HK Proxy" || h.cfg.ProxyPool[0].URL != "http://127.0.0.1:7891" || h.cfg.ProxyPool[0].Enabled {
		t.Fatalf("updated proxy pool = %#v", h.cfg.ProxyPool)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Count(string(data), "id: hk") != 1 {
		t.Fatalf("expected exactly one hk proxy entry after patch:\n%s", string(data))
	}
	if !strings.Contains(string(data), "name: Updated HK Proxy") || !strings.Contains(string(data), "url: http://127.0.0.1:7891") {
		t.Fatalf("persisted config missing updated values:\n%s", string(data))
	}
}

func TestPostProxyPoolCheckUsesConfiguredProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://target.example/generate_204" {
			t.Fatalf("proxy received URL %q", r.URL.String())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "local", Name: "Local", URL: proxyServer.URL, Enabled: true},
		},
	}, "", nil)
	defer h.Close()

	payload := []byte(`{"id":"local","test_url":"http://target.example/generate_204"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/proxy-pool/check", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostProxyPoolCheck(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		OK         bool   `json:"ok"`
		StatusCode int    `json:"statusCode"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.OK || body.StatusCode != http.StatusNoContent {
		t.Fatalf("check response = %#v", body)
	}
	if body.Message != "" {
		t.Fatalf("success message = %q, want empty", body.Message)
	}
}

func TestDefaultProxyCheckURLPrefersForwardedOrigin(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/proxy-pool/check", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Forwarded-Host", "panel.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")

	if got, want := defaultProxyCheckURL(req), "https://panel.example.com/v0/management/public/ping"; got != want {
		t.Fatalf("defaultProxyCheckURL = %q, want %q", got, want)
	}
}

func TestDefaultProxyCheckURLFallbacksForPrivateOrLocalHost(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		host string
	}{
		{"IPv4 private 10.x", "10.126.126.7:8317"},
		{"IPv4 private 172.16.x", "172.20.0.5:8080"},
		{"IPv4 private 192.168.x", "192.168.1.100"},
		{"IPv4 loopback", "127.0.0.1:8317"},
		{"localhost", "localhost:8317"},
		{"local domain", "my-server.local"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/proxy-pool/check", nil)
			req.Host = tc.host
			if got, want := defaultProxyCheckURL(req), defaultProxyCheckFallbackURL; got != want {
				t.Fatalf("defaultProxyCheckURL(%q) = %q, want fallback %q", tc.host, got, want)
			}
		})
	}
}

func TestPostProxyPoolCheckAllowsDisabledProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()

	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "disabled-proxy", Name: "Disabled", URL: proxyServer.URL, Enabled: false},
		},
	}, "", nil)
	defer h.Close()

	payload := []byte(`{"id":"disabled-proxy","test_url":"http://target.example/generate_204"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/proxy-pool/check", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostProxyPoolCheck(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		OK         bool `json:"ok"`
		StatusCode int  `json:"statusCode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.OK || body.StatusCode != http.StatusNoContent {
		t.Fatalf("check response = %#v, want successful probe for disabled entry", body)
	}
}

func TestProxyPoolHandlersUseSQLiteWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := usageTestProxyPoolDB(t)
	defer cleanup()

	if err := usage.ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: "db", Name: "DB Proxy", URL: "http://127.0.0.1:7890", Enabled: true},
	}); err != nil {
		t.Fatalf("ReplaceProxyPool: %v", err)
	}

	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "yaml", Name: "YAML Proxy", URL: "http://127.0.0.1:8888", Enabled: true},
		},
	}, "", nil)
	defer h.Close()
	var hookCalled bool
	var hookProxyPool []config.ProxyPoolEntry
	h.SetConfigMutatedHook(func(updated *config.Config) {
		hookCalled = true
		if updated != nil {
			hookProxyPool = append([]config.ProxyPoolEntry(nil), updated.ProxyPool...)
		}
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.GetProxyPool(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"db"`) || strings.Contains(w.Body.String(), `"id":"yaml"`) {
		t.Fatalf("GetProxyPool should prefer SQLite rows, body = %s", w.Body.String())
	}

	payload := []byte(`{"items":[{"id":"new","name":"New Proxy","url":"http://127.0.0.1:9000","enabled":true}]}`)
	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/proxy-pool", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyPool(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	rows := usage.ListProxyPool()
	if len(rows) != 1 || rows[0].ID != "new" {
		t.Fatalf("SQLite proxy pool = %#v", rows)
	}
	if len(h.cfg.ProxyPool) != 1 || h.cfg.ProxyPool[0].ID != "new" {
		t.Fatalf("runtime proxy pool = %#v", h.cfg.ProxyPool)
	}
	if !hookCalled {
		t.Fatal("config mutation hook was not called")
	}
	if len(hookProxyPool) != 1 || hookProxyPool[0].ID != "new" {
		t.Fatalf("hook proxy pool = %#v", hookProxyPool)
	}
}

func TestPutProxyPoolRejectsDuplicateIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := usageTestProxyPoolDB(t)
	defer cleanup()

	h := NewHandler(&config.Config{}, "", nil)
	defer h.Close()

	payload := []byte(`{"items":[
		{"id":"ip","name":"old","url":"socks5://1.1.1.1:1080","enabled":true},
		{"id":"IP","name":"new","url":"socks5://2.2.2.2:1080","enabled":true}
	]}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/proxy-pool", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PutProxyPool(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "duplicate proxy ids") {
		t.Fatalf("body = %s, want duplicate proxy ids", w.Body.String())
	}
	if rows := usage.ListProxyPool(); len(rows) != 0 {
		t.Fatalf("store should stay empty on rejected put, got %#v", rows)
	}
}

func TestPatchProxyPoolEntryUsesSQLiteWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cleanup := usageTestProxyPoolDB(t)
	defer cleanup()

	if err := usage.ReplaceProxyPool([]config.ProxyPoolEntry{
		{ID: "db", Name: "DB Proxy", URL: "http://127.0.0.1:7890", Enabled: true},
	}); err != nil {
		t.Fatalf("ReplaceProxyPool: %v", err)
	}

	h := NewHandler(&config.Config{
		ProxyPool: []config.ProxyPoolEntry{
			{ID: "yaml", Name: "YAML Proxy", URL: "http://127.0.0.1:8888", Enabled: true},
		},
	}, "", nil)
	defer h.Close()
	var hookCalled bool
	var hookProxyPool []config.ProxyPoolEntry
	h.SetConfigMutatedHook(func(updated *config.Config) {
		hookCalled = true
		if updated != nil {
			hookProxyPool = append([]config.ProxyPoolEntry(nil), updated.ProxyPool...)
		}
	})

	payload := []byte(`{"name":"Updated DB Proxy","url":"http://127.0.0.1:9001","enabled":false,"description":"rotated"}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "db"}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/proxy-pool/db", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchProxyPoolEntry(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	rows := usage.ListProxyPool()
	if len(rows) != 1 {
		t.Fatalf("SQLite proxy pool length = %d, want 1: %#v", len(rows), rows)
	}
	if rows[0].ID != "db" || rows[0].Name != "Updated DB Proxy" || rows[0].URL != "http://127.0.0.1:9001" || rows[0].Enabled {
		t.Fatalf("SQLite proxy pool = %#v", rows)
	}
	if rows[0].Description != "rotated" {
		t.Fatalf("SQLite description = %q, want rotated", rows[0].Description)
	}
	if len(h.cfg.ProxyPool) != 1 || h.cfg.ProxyPool[0].ID != "db" || h.cfg.ProxyPool[0].Name != "Updated DB Proxy" {
		t.Fatalf("runtime proxy pool = %#v", h.cfg.ProxyPool)
	}
	if !hookCalled {
		t.Fatal("config mutation hook was not called")
	}
	if len(hookProxyPool) != 1 || hookProxyPool[0].ID != "db" || hookProxyPool[0].Name != "Updated DB Proxy" {
		t.Fatalf("hook proxy pool = %#v", hookProxyPool)
	}
}

func usageTestProxyPoolDB(t *testing.T) func() {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, nil); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return usage.CloseDB
}

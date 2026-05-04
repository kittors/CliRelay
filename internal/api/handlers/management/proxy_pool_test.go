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
	t.Parallel()
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
	t.Parallel()
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

func TestPostProxyPoolCheckUsesConfiguredProxy(t *testing.T) {
	t.Parallel()
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
}

func usageTestProxyPoolDB(t *testing.T) func() {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, nil); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return usage.CloseDB
}

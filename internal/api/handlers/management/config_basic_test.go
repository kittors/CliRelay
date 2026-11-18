package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/runtimeconfig"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestMaskKey(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"", ""},
		{"short", "****"},
		{"sk-useXXXX1234567890abcd", "sk-use****abcd"},
	}
	for _, tc := range tests {
		if got := maskKey(tc.input); got != tc.expected {
			t.Errorf("maskKey(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestMaskName(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"", ""},
		{"A", "***"},
		{"管理员", "管***"},
		{"测试账号", "测***"},
		{"Alice", "A***"},
	}
	for _, tc := range tests {
		if got := maskName(tc.input); got != tc.expected {
			t.Errorf("maskName(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestMaskBaseURL(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"", ""},
		{"https://api.tabcode.cc/openai", "https://api.tabcode.cc/***"},
		{"https://chat.tabcode.cc", "https://chat.tabcode.cc/***"},
		{"http://localhost:8080/v1/chat", "http://localhost:8080/***"},
		{"not-a-url", "***"},
	}
	for _, tc := range tests {
		if got := maskBaseURL(tc.input); got != tc.expected {
			t.Errorf("maskBaseURL(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestSanitizeConfigForAPI(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			ProxyURL: "http://proxy.example.com:8080/path",
			APIKeys:  []string{"sk-useXXXX1234567890abcd"},
			APIKeyEntries: []config.APIKeyEntry{
				{Key: "sk-useXXXX1234567890abcd", Name: "管理员"},
				{Key: "sk-gauXXXXb41v00000000", Name: "测试账号"},
			},
		},
		Redis: config.RedisConfig{
			Enable:   true,
			Addr:     "127.0.0.1:6379",
			Password: "secret",
		},
		TLS: config.TLSConfig{
			Enable: false,
			Cert:   "/etc/ssl/cert.pem",
			Key:    "/etc/ssl/key.pem",
		},
		Pprof: config.PprofConfig{
			Addr: "127.0.0.1:8316",
		},
		GeminiKey: []config.GeminiKey{
			{
				APIKey:  "sk-mJdvXXXXm0dV000000",
				Name:    "Gemini公益",
				BaseURL: "https://chat.tabcode.cc",
				Models: []config.GeminiModel{
					{Name: "gemini-3.1-flash", Alias: ""},
				},
				ExcludedModels: []string{"gemini-old-model"},
			},
		},
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "sk-useXXXX586d000000",
				Name:    "GLM渠道",
				BaseURL: "https://api.tabcode.cc/claude/glm",
				Models: []config.ClaudeModel{
					{Name: "glm-5", Alias: ""},
				},
				ExcludedModels: []string{"*"},
			},
		},
		CodexKey: []config.CodexKey{
			{
				APIKey:  "sk-useXXXXdb42000000",
				Name:    "codex team 代理",
				BaseURL: "https://api.tabcode.cc/openai",
				Models: []config.CodexModel{
					{Name: "gpt-5.4", Alias: ""},
				},
				ExcludedModels: []string{"gpt-old"},
			},
		},
		OpenCodeGoKey: []config.OpenCodeGoKey{
			{
				APIKey:         "sk-goXXXX1234000000",
				Name:           "OpenCode Go 渠道",
				ProxyURL:       "https://proxy.example.com/go",
				ExcludedModels: []string{"minimax-m2.5"},
			},
		},
		ClineKey: []config.ClineKey{
			{
				APIKey:         "sk-clineXXXX1234000000",
				Name:           "ClinePass 渠道",
				BaseURL:        "https://api.cline.bot/api/v1",
				AuthCookie:     "cline-cookie-secret",
				ExcludedModels: []string{"cline-pass/old-model"},
			},
		},
		OllamaCloudKey: []config.OllamaCloudKey{
			{
				APIKey:         "sk-ollamaXXXX1234000000",
				Name:           "Ollama Cloud 渠道",
				BaseURL:        "https://ollama.com",
				AuthCookie:     "ollama-cookie-secret",
				ExcludedModels: []string{"old-model"},
			},
		},
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"antigravity": {
				{Name: "rev19-uic3-1p", Alias: "gemini-2.5-computer-use-preview"},
			},
		},
		OAuthExcludedModels: map[string][]string{
			"anthropic": {"claude-old-model"},
		},
		AmpCode: config.AmpCode{
			UpstreamURL:    "https://amp.example.com/api",
			UpstreamAPIKey: "sk-ampXXXX1234000000",
			ModelMappings: []config.AmpModelMapping{
				{From: "claude-opus-4.5", To: "claude-sonnet-4"},
			},
		},
	}

	sanitized := sanitizeConfigForAPI(cfg)

	// ── Verify API keys are masked ──
	for _, k := range sanitized.APIKeys {
		if k == "sk-useXXXX1234567890abcd" {
			t.Error("APIKeys: raw key not masked")
		}
	}

	// ── Verify API key entry names are masked ──
	for _, e := range sanitized.APIKeyEntries {
		if e.Name == "管理员" || e.Name == "测试账号" {
			t.Errorf("APIKeyEntries: name %q not masked", e.Name)
		}
		if e.Key == "sk-useXXXX1234567890abcd" || e.Key == "sk-gauXXXXb41v00000000" {
			t.Errorf("APIKeyEntries: key %q not masked", e.Key)
		}
	}

	// ── Verify Gemini keys ──
	for _, g := range sanitized.GeminiKey {
		if g.Name == "Gemini公益" {
			t.Error("GeminiKey: name not masked")
		}
		if g.BaseURL == "https://chat.tabcode.cc" {
			t.Error("GeminiKey: base-url not masked")
		}
		if g.Models != nil {
			t.Error("GeminiKey: models not cleared")
		}
	}

	// ── Verify Claude keys ──
	for _, c := range sanitized.ClaudeKey {
		if c.Name == "GLM渠道" {
			t.Error("ClaudeKey: name not masked")
		}
		if c.BaseURL == "https://api.tabcode.cc/claude/glm" {
			t.Error("ClaudeKey: base-url not masked")
		}
		if c.Models != nil {
			t.Error("ClaudeKey: models not cleared")
		}
	}

	// ── Verify Codex keys ──
	for _, c := range sanitized.CodexKey {
		if c.Name == "codex team 代理" {
			t.Error("CodexKey: name not masked")
		}
		if c.BaseURL == "https://api.tabcode.cc/openai" {
			t.Error("CodexKey: base-url not masked")
		}
		if c.Models != nil {
			t.Error("CodexKey: models not cleared")
		}
	}

	// ── Verify OpenCode Go keys ──
	for _, c := range sanitized.OpenCodeGoKey {
		if c.APIKey == "sk-goXXXX1234000000" {
			t.Error("OpenCodeGoKey: api-key not masked")
		}
		if c.Name == "OpenCode Go 渠道" {
			t.Error("OpenCodeGoKey: name not masked")
		}
		if c.ProxyURL == "https://proxy.example.com/go" {
			t.Error("OpenCodeGoKey: proxy-url not masked")
		}
	}

	for _, c := range sanitized.ClineKey {
		if c.AuthCookie == "cline-cookie-secret" {
			t.Error("ClineKey: auth-cookie not masked")
		}
	}
	for _, c := range sanitized.OllamaCloudKey {
		if c.AuthCookie == "ollama-cookie-secret" {
			t.Error("OllamaCloudKey: auth-cookie not masked")
		}
	}

	// ── Verify OAuthModelAlias is stripped ──
	if sanitized.OAuthModelAlias != nil {
		t.Error("OAuthModelAlias not cleared")
	}

	// ── Verify OAuthExcludedModels is stripped ──
	if sanitized.OAuthExcludedModels != nil {
		t.Error("OAuthExcludedModels not cleared")
	}

	// ── Verify Provider ExcludedModels are stripped ──
	for _, g := range sanitized.GeminiKey {
		if g.ExcludedModels != nil {
			t.Error("GeminiKey: excluded-models not cleared")
		}
	}
	for _, c := range sanitized.ClaudeKey {
		if c.ExcludedModels != nil {
			t.Error("ClaudeKey: excluded-models not cleared")
		}
	}
	for _, c := range sanitized.CodexKey {
		if c.ExcludedModels != nil {
			t.Error("CodexKey: excluded-models not cleared")
		}
	}
	for _, c := range sanitized.OpenCodeGoKey {
		if c.ExcludedModels != nil {
			t.Error("OpenCodeGoKey: excluded-models not cleared")
		}
	}

	// ── Verify Redis addr is masked ──
	if sanitized.Redis.Addr == "127.0.0.1:6379" {
		t.Error("Redis.Addr not masked")
	}
	if sanitized.Redis.Password != "***" {
		t.Error("Redis.Password not masked")
	}

	// ── Verify pprof addr is masked ──
	if sanitized.Pprof.Addr == "127.0.0.1:8316" {
		t.Error("Pprof.Addr not masked")
	}

	// ── Verify TLS paths are masked ──
	if sanitized.TLS.Cert == "/etc/ssl/cert.pem" {
		t.Error("TLS.Cert not masked")
	}
	if sanitized.TLS.Key == "/etc/ssl/key.pem" {
		t.Error("TLS.Key not masked")
	}

	// ── Verify Amp upstream is masked ──
	if sanitized.AmpCode.UpstreamURL == "https://amp.example.com/api" {
		t.Error("AmpCode.UpstreamURL not masked")
	}
	if sanitized.AmpCode.ModelMappings != nil {
		t.Error("AmpCode.ModelMappings not cleared")
	}

	// ── Verify global ProxyURL is masked ──
	if sanitized.ProxyURL == "http://proxy.example.com:8080/path" {
		t.Error("ProxyURL not masked")
	}

	// ── Verify original config is NOT modified ──
	if cfg.Redis.Addr != "127.0.0.1:6379" {
		t.Error("original config was mutated: Redis.Addr")
	}
	if cfg.APIKeyEntries[0].Name != "管理员" {
		t.Error("original config was mutated: APIKeyEntries[0].Name")
	}
	if cfg.OAuthModelAlias == nil {
		t.Error("original config was mutated: OAuthModelAlias")
	}
}

func TestPutConfigYAMLRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := bytes.Repeat([]byte("a"), int(bodyutil.ConfigYAMLBodyLimit)+1)
	c.Request = httptest.NewRequest(http.MethodPut, "/config", bytes.NewReader(body))

	h := &Handler{}
	h.PutConfigYAML(c)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
	}
}

func TestPutConfigYAMLUpdatesExistingStoredPayloadRuntimeSetting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := settingsstore.UpsertRuntimeSetting(settingsstore.RuntimeSettingPayload, config.PayloadConfig{
		Override: []config.PayloadRule{
			{
				Models: []config.PayloadModelRule{{Name: "old-model", Protocol: "codex"}},
				Params: map[string]any{"service_tier": "standard"},
			},
		},
	}); err != nil {
		t.Fatalf("seed payload runtime setting: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8318\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	body := []byte(`port: 8318
payload:
  override:
    - models:
        - name: gpt-5.4
          protocol: codex
        - name: gpt-5.4-mini
          protocol: codex
      params:
        service_tier: priority
logging-to-file: true
`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", bytes.NewReader(body))

	h := NewHandler(&config.Config{}, configPath, nil)
	h.PutConfigYAML(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
	}
	assertPayloadOverrideRule(t, h.cfg.Payload)

	var stored config.Config
	if !settingsstore.ApplyStoredRuntimeSettings(&stored) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	assertPayloadOverrideRule(t, stored.Payload)

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "payload:") {
		t.Fatalf("payload should be migrated out of YAML after save:\n%s", string(data))
	}
}

func TestPutConfigYAMLNotifiesConfigMutationHookAfterPayloadRuntimeSettingUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	dbPath := filepath.Join(t.TempDir(), "usage.sqlite")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	if err := settingsstore.UpsertRuntimeSetting(settingsstore.RuntimeSettingPayload, config.PayloadConfig{
		Override: []config.PayloadRule{
			{
				Models: []config.PayloadModelRule{{Name: "old-model", Protocol: "codex"}},
				Params: map[string]any{"service_tier": "standard"},
			},
		},
	}); err != nil {
		t.Fatalf("seed payload runtime setting: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8318\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	body := []byte(`port: 8318
payload:
  override:
    - models:
        - name: gpt-5.4
          protocol: codex
        - name: gpt-5.4-mini
          protocol: codex
      params:
        service_tier: priority
logging-to-file: true
`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", bytes.NewReader(body))

	var hookCalled bool
	var hookPayload config.PayloadConfig
	h := NewHandler(&config.Config{}, configPath, nil)
	h.SetConfigMutatedHook(func(updated *config.Config) {
		hookCalled = true
		if updated != nil {
			hookPayload = updated.Payload
		}
	})
	h.PutConfigYAML(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !hookCalled {
		t.Fatal("config mutation hook was not called")
	}
	assertPayloadOverrideRule(t, hookPayload)
}

// A runtime setting spelled out in a config.yaml save is owned by the
// database: every later reload applies the stored value and strips the key
// from config.yaml. The save used to log a failed write of such a key and
// still answer 200, after which the new value existed nowhere.
func TestPutConfigYAMLRejectsSaveWhenRuntimeSettingIsNotStored(t *testing.T) {
	h, configPath, original := newRuntimeSettingYAMLSaveFixture(t)
	var hookCalls int
	h.SetConfigMutatedHook(func(*config.Config) { hookCalls++ })
	// The database refuses the proxy-url row while every read still works, like
	// a statement failing mid-transaction. request-retry alone would be fine.
	restore := failRuntimeSettingWrites(t, runtimeconfig.RuntimeSettingProxyURL, errors.New("injected: connection reset while storing proxy-url"))
	body := []byte("# operator notes\nport: 8319\nrequest-retry: 5\nproxy-url: " + yamlSaveNewProxy + "\n")

	rec := putConfigYAML(h, body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PutConfigYAML status = %d body = %s, want 500", rec.Code, rec.Body.String())
	}
	// Nothing of the rejected save may stick, or a retry would start from a
	// half-applied state: not the other key, not the file, not the live config.
	assertStoredProxyURL(t, yamlSaveOldProxy)
	if payload, ok := settingsstore.GetRuntimeSettingPayload(runtimeconfig.RuntimeSettingRequestRetry); ok {
		t.Fatalf("request-retry = %s was stored by a save that failed", payload)
	}
	assertFileContent(t, configPath, original)
	if h.cfg.ProxyURL != yamlSaveOldProxy || h.cfg.RequestRetry != 0 || hookCalls != 0 {
		t.Fatalf("live config changed by a failed save: proxy-url=%q request-retry=%d hook calls=%d", h.cfg.ProxyURL, h.cfg.RequestRetry, hookCalls)
	}

	// The operator retries the same YAML once the database takes writes again.
	restore()
	rec = putConfigYAML(h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("retried PutConfigYAML status = %d body = %s", rec.Code, rec.Body.String())
	}
	assertStoredProxyURL(t, yamlSaveNewProxy)
	if h.cfg.ProxyURL != yamlSaveNewProxy || h.cfg.RequestRetry != 5 || h.cfg.Port != 8319 {
		t.Fatalf("live config after retry: proxy-url=%q request-retry=%d port=%d", h.cfg.ProxyURL, h.cfg.RequestRetry, h.cfg.Port)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "proxy-url") || strings.Contains(string(data), "request-retry") || !strings.Contains(string(data), "port: 8319") {
		t.Fatalf("config.yaml after the stored save should keep port and drop the stored keys:\n%s", data)
	}
}

// A key another writer keeps changing exhausts the retries; that is a version
// conflict, answered like every other conflicting configuration write.
func TestPutConfigYAMLAnswersConflictWhenRuntimeSettingKeepsChanging(t *testing.T) {
	h, configPath, original := newRuntimeSettingYAMLSaveFixture(t)
	key := runtimeconfig.RuntimeSettingProxyURL
	failRuntimeSettingWrites(t, key, &configsync.ConflictError{Domain: configsync.RuntimeSettingDomain(key), Key: key, Expected: 1, Current: 2})

	rec := putConfigYAML(h, []byte("# operator notes\nport: 8318\nproxy-url: "+yamlSaveNewProxy+"\n"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("PutConfigYAML status = %d body = %s, want 409", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error.Code != "config_version_conflict" {
		t.Fatalf("conflict body = %s (%v), want code config_version_conflict", rec.Body.String(), err)
	}
	assertStoredProxyURL(t, yamlSaveOldProxy)
	assertFileContent(t, configPath, original)
	if h.cfg.ProxyURL != yamlSaveOldProxy {
		t.Fatalf("live proxy-url = %q after a rejected save", h.cfg.ProxyURL)
	}
}

// The database took the runtime settings, then config.yaml could not be
// written. The other nodes apply the stored value from the published event;
// this node never receives its own event, so it must adopt the value itself
// instead of serving the old one until some later reload.
func TestPutConfigYAMLKeepsStoredRuntimeSettingsLiveWhenFileWriteFails(t *testing.T) {
	h, configPath, _ := newRuntimeSettingYAMLSaveFixture(t)
	// A directory in config.yaml's place: validation, which writes a temp file
	// next to it, passes, and only the write of config.yaml itself fails.
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var hooked *config.Config
	h.SetConfigMutatedHook(func(cfg *config.Config) { hooked = cfg })

	rec := putConfigYAML(h, []byte("port: 8318\nproxy-url: "+yamlSaveNewProxy+"\n"))

	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), runtimeconfig.RuntimeSettingProxyURL) {
		t.Fatalf("PutConfigYAML status = %d body = %s, want 500 naming the stored proxy-url", rec.Code, rec.Body.String())
	}
	assertStoredProxyURL(t, yamlSaveNewProxy)
	storedVersion := settingsstore.RuntimeSettingVersion("", runtimeconfig.RuntimeSettingProxyURL)
	if h.cfg.ProxyURL != yamlSaveNewProxy || h.cfg.RuntimeSettingState().Version(runtimeconfig.RuntimeSettingProxyURL) != storedVersion {
		t.Fatalf("live proxy-url = %q at version %d, want the stored %q at %d", h.cfg.ProxyURL, h.cfg.RuntimeSettingState().Version(runtimeconfig.RuntimeSettingProxyURL), yamlSaveNewProxy, storedVersion)
	}
	if hooked == nil || hooked != h.cfg {
		t.Fatal("config mutation hook was not notified with the live config")
	}
}

const (
	yamlSaveOldProxy = "http://old-proxy.example:8080"
	yamlSaveNewProxy = "http://new-proxy.example:8080"
)

// newRuntimeSettingYAMLSaveFixture is a node whose database already owns
// proxy-url and whose config.yaml therefore holds node-local settings only.
func newRuntimeSettingYAMLSaveFixture(t *testing.T) (*Handler, string, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	usage.CloseDB()
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.sqlite"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	if err := settingsstore.UpsertRuntimeSetting(runtimeconfig.RuntimeSettingProxyURL, yamlSaveOldProxy); err != nil {
		t.Fatalf("seed proxy-url: %v", err)
	}
	live := &config.Config{}
	settingsstore.ApplyStoredRuntimeSettings(live)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("# operator notes\nport: 8318\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return NewHandler(live, configPath, nil), configPath, original
}

// failRuntimeSettingWrites fails every write of key inside its transaction,
// before the commit, and leaves reads and other keys alone. It returns a
// function that lifts the failure early; cleanup lifts it as well.
func failRuntimeSettingWrites(t *testing.T, key string, err error) func() {
	t.Helper()
	restore := configsync.SetPublisherForTest(func(_ context.Context, _ *sql.Tx, ev cluster.ConfigEvent) error {
		if ev.Key == key {
			return err
		}
		return nil
	})
	t.Cleanup(restore)
	return restore
}

func putConfigYAML(h *Handler, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/config.yaml", bytes.NewReader(body))
	h.PutConfigYAML(c)
	return rec
}

func assertStoredProxyURL(t *testing.T, want string) {
	t.Helper()
	var stored config.Config
	settingsstore.ApplyStoredRuntimeSettings(&stored)
	if stored.ProxyURL != want {
		t.Fatalf("stored proxy-url = %q, want %q", stored.ProxyURL, want)
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("%s changed by a failed save:\n%s\nwant:\n%s", filepath.Base(path), data, want)
	}
}

func assertPayloadOverrideRule(t *testing.T, payload config.PayloadConfig) {
	t.Helper()
	if len(payload.Override) != 1 {
		t.Fatalf("payload override rule count = %d, want 1: %#v", len(payload.Override), payload.Override)
	}
	rule := payload.Override[0]
	if len(rule.Models) != 2 {
		t.Fatalf("payload override model count = %d, want 2: %#v", len(rule.Models), rule.Models)
	}
	if rule.Models[0].Name != "gpt-5.4" || rule.Models[0].Protocol != "codex" {
		t.Fatalf("first payload model = %#v", rule.Models[0])
	}
	if rule.Models[1].Name != "gpt-5.4-mini" || rule.Models[1].Protocol != "codex" {
		t.Fatalf("second payload model = %#v", rule.Models[1])
	}
	if got := rule.Params["service_tier"]; got != "priority" {
		t.Fatalf("service_tier = %#v, want priority", got)
	}
}

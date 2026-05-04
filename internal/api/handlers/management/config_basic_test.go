package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

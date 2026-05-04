package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	latestReleaseURL       = "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest"
	latestReleaseUserAgent = "CLIProxyAPI"
)

func (h *Handler) GetConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{})
		return
	}
	c.JSON(200, sanitizeConfigForAPI(h.cfg))
}

// maskKey masks an API key / secret, preserving first 6 and last 4 characters.
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 12 {
		return "****"
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// maskName masks a person's or channel's name, keeping the first rune + "***".
func maskName(name string) string {
	if name == "" {
		return ""
	}
	runes := []rune(name)
	if len(runes) <= 1 {
		return "***"
	}
	return string(runes[0:1]) + "***"
}

// maskBaseURL masks a URL, preserving the scheme + host but replacing the path with /***.
func maskBaseURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "***"
	}
	return u.Scheme + "://" + u.Host + "/***"
}

// sanitizeConfigForAPI creates a sanitized copy of the config suitable for the management API.
// It masks provider API keys, Redis passwords, user names, provider URLs/models, and other sensitive fields.
func sanitizeConfigForAPI(cfg *config.Config) *config.Config {
	// Deep copy via JSON round-trip to avoid mutating the live config.
	data, err := json.Marshal(cfg)
	if err != nil {
		return cfg // fallback to original on error
	}
	var copy config.Config
	if err := json.Unmarshal(data, &copy); err != nil {
		return cfg
	}

	// ── Infrastructure ──────────────────────────────────────────────────────

	// Mask Redis password and address
	if copy.Redis.Password != "" {
		copy.Redis.Password = "***"
	}
	if copy.Redis.Addr != "" {
		copy.Redis.Addr = "***"
	}

	// Mask pprof address
	copy.Pprof.Addr = "***"

	// Mask TLS cert/key paths
	if copy.TLS.Cert != "" {
		copy.TLS.Cert = "***"
	}
	if copy.TLS.Key != "" {
		copy.TLS.Key = "***"
	}

	// ── Provider channels ──────────────────────────────────────────────────

	// Mask Gemini API keys, names, URLs, and models
	for i := range copy.GeminiKey {
		copy.GeminiKey[i].APIKey = maskKey(copy.GeminiKey[i].APIKey)
		copy.GeminiKey[i].Name = maskName(copy.GeminiKey[i].Name)
		copy.GeminiKey[i].BaseURL = maskBaseURL(copy.GeminiKey[i].BaseURL)
		copy.GeminiKey[i].ProxyURL = maskBaseURL(copy.GeminiKey[i].ProxyURL)
		copy.GeminiKey[i].Models = nil
		copy.GeminiKey[i].ExcludedModels = nil
	}

	// Mask Claude API keys, names, URLs, and models
	for i := range copy.ClaudeKey {
		copy.ClaudeKey[i].APIKey = maskKey(copy.ClaudeKey[i].APIKey)
		copy.ClaudeKey[i].Name = maskName(copy.ClaudeKey[i].Name)
		copy.ClaudeKey[i].BaseURL = maskBaseURL(copy.ClaudeKey[i].BaseURL)
		copy.ClaudeKey[i].ProxyURL = maskBaseURL(copy.ClaudeKey[i].ProxyURL)
		copy.ClaudeKey[i].Models = nil
		copy.ClaudeKey[i].ExcludedModels = nil
	}

	// Mask Codex API keys, names, URLs, and models
	for i := range copy.CodexKey {
		copy.CodexKey[i].APIKey = maskKey(copy.CodexKey[i].APIKey)
		copy.CodexKey[i].Name = maskName(copy.CodexKey[i].Name)
		copy.CodexKey[i].BaseURL = maskBaseURL(copy.CodexKey[i].BaseURL)
		copy.CodexKey[i].ProxyURL = maskBaseURL(copy.CodexKey[i].ProxyURL)
		copy.CodexKey[i].Models = nil
		copy.CodexKey[i].ExcludedModels = nil
	}

	// Mask OpenCode Go API keys, names, proxy URLs, and exclusions
	for i := range copy.OpenCodeGoKey {
		copy.OpenCodeGoKey[i].APIKey = maskKey(copy.OpenCodeGoKey[i].APIKey)
		copy.OpenCodeGoKey[i].Name = maskName(copy.OpenCodeGoKey[i].Name)
		copy.OpenCodeGoKey[i].ProxyURL = maskBaseURL(copy.OpenCodeGoKey[i].ProxyURL)
		copy.OpenCodeGoKey[i].ExcludedModels = nil
	}

	// Mask OpenAI compatibility API keys, names, URLs, and models
	for i := range copy.OpenAICompatibility {
		copy.OpenAICompatibility[i].Name = maskName(copy.OpenAICompatibility[i].Name)
		copy.OpenAICompatibility[i].BaseURL = maskBaseURL(copy.OpenAICompatibility[i].BaseURL)
		copy.OpenAICompatibility[i].Models = nil
		for j := range copy.OpenAICompatibility[i].APIKeyEntries {
			copy.OpenAICompatibility[i].APIKeyEntries[j].APIKey = maskKey(copy.OpenAICompatibility[i].APIKeyEntries[j].APIKey)
			copy.OpenAICompatibility[i].APIKeyEntries[j].ProxyURL = maskBaseURL(copy.OpenAICompatibility[i].APIKeyEntries[j].ProxyURL)
		}
	}

	// Mask Vertex API keys, URLs, and models
	for i := range copy.VertexCompatAPIKey {
		copy.VertexCompatAPIKey[i].APIKey = maskKey(copy.VertexCompatAPIKey[i].APIKey)
		copy.VertexCompatAPIKey[i].BaseURL = maskBaseURL(copy.VertexCompatAPIKey[i].BaseURL)
		copy.VertexCompatAPIKey[i].ProxyURL = maskBaseURL(copy.VertexCompatAPIKey[i].ProxyURL)
		copy.VertexCompatAPIKey[i].Models = nil
	}

	// ── Amp module ──────────────────────────────────────────────────────────

	copy.AmpCode.UpstreamURL = maskBaseURL(copy.AmpCode.UpstreamURL)
	copy.AmpCode.UpstreamAPIKey = maskKey(copy.AmpCode.UpstreamAPIKey)
	for i := range copy.AmpCode.UpstreamAPIKeys {
		copy.AmpCode.UpstreamAPIKeys[i].UpstreamAPIKey = maskKey(copy.AmpCode.UpstreamAPIKeys[i].UpstreamAPIKey)
	}
	copy.AmpCode.ModelMappings = nil

	// ── User-facing API keys ────────────────────────────────────────────────

	for i := range copy.APIKeys {
		copy.APIKeys[i] = maskKey(copy.APIKeys[i])
	}
	for i := range copy.APIKeyEntries {
		copy.APIKeyEntries[i].Key = maskKey(copy.APIKeyEntries[i].Key)
		copy.APIKeyEntries[i].Name = maskName(copy.APIKeyEntries[i].Name)
	}

	// ── OAuth model alias & excluded models (internal mapping, no reason to expose) ──
	copy.OAuthModelAlias = nil
	copy.OAuthExcludedModels = nil

	// ── Global proxy URL ────────────────────────────────────────────────────
	copy.ProxyURL = maskBaseURL(copy.ProxyURL)
	for i := range copy.ProxyPool {
		copy.ProxyPool[i].URL = maskProxyPoolURL(copy.ProxyPool[i].URL)
	}

	return &copy
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected_status", "message": fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}

	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "decode_failed", "message": errDecode.Error()})
		return
	}

	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_response", "message": "missing release version"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"latest-version": version})
}

func WriteConfig(path string, data []byte) error {
	data = config.NormalizeCommentIndentation(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	body, err := bodyutil.ReadRequestBody(c, bodyutil.ConfigYAMLBodyLimit)
	if err != nil {
		if bodyutil.IsTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large", "message": "request body exceeds limit"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	var cfg config.Config
	if err = yaml.Unmarshal(body, &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
		return
	}
	// Validate config using LoadConfigOptional with optional=false to enforce parsing
	tmpDir := filepath.Dir(h.configFilePath)
	tmpFile, err := os.CreateTemp(tmpDir, "config-validate-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	tempFile := tmpFile.Name()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		return
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errClose.Error()})
		return
	}
	defer func() {
		_ = os.Remove(tempFile)
	}()
	_, err = config.LoadConfigOptional(tempFile, false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.List()
	}
	if err = validateRoutingAndAPIKeyRestrictions(&cfg, auths); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if WriteConfig(h.configFilePath, body) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		return
	}
	if usage.ConfigStoreAvailable() {
		usage.MigrateRuntimeSettingsFromConfig(newCfg, h.configFilePath)
		usage.ApplyStoredRuntimeSettings(newCfg)
	}
	h.cfg = newCfg
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
}

// GetConfigYAML returns the raw config.yaml file bytes without re-encoding.
// It preserves comments and original formatting/styles.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// Write raw bytes as-is
	_, _ = c.Writer.Write(data)
}

// Debug
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// UsageStatisticsEnabled
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	c.JSON(200, gin.H{"usage-statistics-enabled": h.cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.UsageStatisticsEnabled = v })
}

// UsageStatisticsEnabled
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	c.JSON(200, gin.H{"logging-to-file": h.cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.LoggingToFile = v })
}

// LogsMaxTotalSizeMB
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	c.JSON(200, gin.H{"logs-max-total-size-mb": h.cfg.LogsMaxTotalSizeMB})
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 0
	}
	h.cfg.LogsMaxTotalSizeMB = value
	h.persist(c)
}

// ErrorLogsMaxFiles
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	c.JSON(200, gin.H{"error-logs-max-files": h.cfg.ErrorLogsMaxFiles})
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	h.cfg.ErrorLogsMaxFiles = value
	h.persist(c)
}

// Request log
func (h *Handler) GetRequestLog(c *gin.Context) { c.JSON(200, gin.H{"request-log": h.cfg.RequestLog}) }
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.RequestLog = v })
}

// Websocket auth
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// Request retry
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// Max retry interval
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// ForceModelPrefix
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// RoutingStrategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	if !ok {
		c.JSON(200, gin.H{"strategy": strings.TrimSpace(h.cfg.Routing.Strategy)})
		return
	}
	c.JSON(200, gin.H{"strategy": strategy})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	var body struct {
		Value *string `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	normalized, ok := normalizeRoutingStrategy(*body.Value)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
		return
	}
	h.cfg.Routing.Strategy = normalized
	h.persist(c)
}

// Proxy URL
func (h *Handler) GetProxyURL(c *gin.Context) { c.JSON(200, gin.H{"proxy-url": h.cfg.ProxyURL}) }
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.ProxyURL = v })
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	h.cfg.ProxyURL = ""
	h.persist(c)
}

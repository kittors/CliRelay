package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type identityFingerprintResponse struct {
	IdentityFingerprint config.IdentityFingerprintConfig                      `json:"identity-fingerprint"`
	Defaults            config.IdentityFingerprintConfig                      `json:"defaults"`
	Learned             map[string][]identityfingerprint.LearnedRecord        `json:"learned"`
	Effective           map[string][]identityfingerprint.EffectiveFingerprint `json:"effective"`
	Status              map[string]identityFingerprintProviderStatus          `json:"status"`
}

type identityFingerprintProviderStatus struct {
	Enabled      bool `json:"enabled"`
	LearnedCount int  `json:"learned_count"`
}

func (h *Handler) GetIdentityFingerprint(c *gin.Context) {
	current := h.currentIdentityFingerprintConfig()
	learned, effective := h.identityFingerprintState(current)
	c.JSON(http.StatusOK, identityFingerprintResponse{
		IdentityFingerprint: current,
		Defaults: config.IdentityFingerprintConfig{
			Codex:  config.DefaultCodexIdentityFingerprint(),
			Claude: config.DefaultClaudeIdentityFingerprint(),
			Gemini: config.DefaultGeminiIdentityFingerprint(),
		},
		Learned:   learned,
		Effective: effective,
		Status: map[string]identityFingerprintProviderStatus{
			"claude": {Enabled: current.Claude.Enabled, LearnedCount: len(learned["claude"])},
			"codex":  {Enabled: current.Codex.Enabled, LearnedCount: len(learned["codex"])},
			"gemini": {Enabled: current.Gemini.Enabled, LearnedCount: len(learned["gemini"])},
		},
	})
}

func (h *Handler) PutIdentityFingerprint(c *gin.Context) {
	var body config.IdentityFingerprintConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	body.Codex = config.CleanCodexIdentityFingerprint(body.Codex)
	body.Claude = config.CleanClaudeIdentityFingerprint(body.Claude)
	body.Gemini = config.CleanGeminiIdentityFingerprint(body.Gemini)
	if err := validateCodexIdentityFingerprint(body.Codex); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateClaudeIdentityFingerprint(body.Claude); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := validateGeminiIdentityFingerprint(body.Gemini); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Codex.Enabled && body.Codex.SessionMode == "fixed" && strings.TrimSpace(body.Codex.SessionID) == "" {
		body.Codex.SessionID = uuid.NewString()
	}
	if body.Claude.Enabled && body.Claude.SessionMode == "fixed" && strings.TrimSpace(body.Claude.SessionID) == "" {
		body.Claude.SessionID = uuid.NewString()
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	previous := h.cfg.IdentityFingerprint
	h.cfg.IdentityFingerprint = body
	h.mu.Unlock()

	if !h.persistRuntimeSetting(c, settingsstore.RuntimeSettingIdentityFingerprint, body) {
		h.mu.Lock()
		if h.cfg != nil {
			h.cfg.IdentityFingerprint = previous
		}
		h.mu.Unlock()
		return
	}
}

func (h *Handler) GetCodexFingerprintRecommendations(c *gin.Context) {
	result, err := usage.QueryCodexFingerprintRecommendations(usage.CodexFingerprintRecommendationQuery{
		Days:  intQueryDefault(c, "days", 7),
		Limit: intQueryDefault(c, "limit", 200),
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) DeleteIdentityFingerprintLearned(c *gin.Context) {
	provider := identityfingerprint.Provider(strings.TrimSpace(c.Query("provider")))
	accountKey := strings.TrimSpace(c.Query("account_key"))
	if provider == "" || accountKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider and account_key are required"})
		return
	}
	deleted, err := usage.DeleteIdentityFingerprint(provider, accountKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) identityFingerprintState(current config.IdentityFingerprintConfig) (map[string][]identityfingerprint.LearnedRecord, map[string][]identityfingerprint.EffectiveFingerprint) {
	learned := map[string][]identityfingerprint.LearnedRecord{
		"claude": {},
		"codex":  {},
		"gemini": {},
	}
	effective := map[string][]identityfingerprint.EffectiveFingerprint{
		"claude": {},
		"codex":  {},
		"gemini": {},
	}
	for _, provider := range []identityfingerprint.Provider{identityfingerprint.ProviderClaude, identityfingerprint.ProviderCodex, identityfingerprint.ProviderGemini} {
		records, err := usage.ListIdentityFingerprints(provider, 200)
		if err != nil {
			continue
		}
		key := string(provider)
		learned[key] = records
		for i := range records {
			record := records[i]
			switch provider {
			case identityfingerprint.ProviderClaude:
				_, eff := identityfingerprint.ResolveClaude(current.Claude, &record)
				effective[key] = append(effective[key], eff)
			case identityfingerprint.ProviderCodex:
				_, eff := identityfingerprint.ResolveCodex(current.Codex, &record)
				effective[key] = append(effective[key], eff)
			case identityfingerprint.ProviderGemini:
				_, eff := identityfingerprint.ResolveGemini(current.Gemini, &record)
				effective[key] = append(effective[key], eff)
			}
		}
	}
	return learned, effective
}

func validateCodexIdentityFingerprint(fp config.CodexIdentityFingerprintConfig) error {
	if containsHeaderLineBreak(fp.UserAgent) || containsHeaderLineBreak(fp.Version) ||
		containsHeaderLineBreak(fp.Originator) || containsHeaderLineBreak(fp.WebsocketBeta) ||
		containsHeaderLineBreak(fp.BetaFeatures) || containsHeaderLineBreak(fp.SessionID) {
		return fmt.Errorf("identity fingerprint fields must not contain line breaks")
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("custom header name cannot be empty")
		}
		if !isHTTPHeaderToken(key) {
			return fmt.Errorf("invalid custom header name: %s", key)
		}
		if isIdentityFingerprintBlockedHeader(key) {
			return fmt.Errorf("custom header %s is managed by the system", key)
		}
		if containsHeaderLineBreak(value) {
			return fmt.Errorf("custom header %s must not contain line breaks", key)
		}
	}
	return nil
}

func validateClaudeIdentityFingerprint(fp config.ClaudeIdentityFingerprintConfig) error {
	if containsHeaderLineBreak(fp.UserAgent) || containsHeaderLineBreak(fp.CLIVersion) ||
		containsHeaderLineBreak(fp.Entrypoint) || containsHeaderLineBreak(fp.AnthropicBeta) ||
		containsHeaderLineBreak(fp.StainlessPackageVersion) || containsHeaderLineBreak(fp.StainlessRuntimeVersion) ||
		containsHeaderLineBreak(fp.StainlessTimeout) || containsHeaderLineBreak(fp.SessionID) ||
		containsHeaderLineBreak(fp.DeviceID) {
		return fmt.Errorf("identity fingerprint fields must not contain line breaks")
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("custom header name cannot be empty")
		}
		if !isHTTPHeaderToken(key) {
			return fmt.Errorf("invalid custom header name: %s", key)
		}
		if isIdentityFingerprintBlockedHeader(key) || isClaudeIdentityFingerprintBlockedHeader(key) {
			return fmt.Errorf("custom header %s is managed by the system", key)
		}
		if containsHeaderLineBreak(value) {
			return fmt.Errorf("custom header %s must not contain line breaks", key)
		}
	}
	return nil
}

func validateGeminiIdentityFingerprint(fp config.GeminiIdentityFingerprintConfig) error {
	if containsHeaderLineBreak(fp.UserAgent) || containsHeaderLineBreak(fp.APIClient) ||
		containsHeaderLineBreak(fp.ClientMetadata) {
		return fmt.Errorf("identity fingerprint fields must not contain line breaks")
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return fmt.Errorf("custom header name cannot be empty")
		}
		if !isHTTPHeaderToken(key) {
			return fmt.Errorf("invalid custom header name: %s", key)
		}
		if isIdentityFingerprintBlockedHeader(key) || isGeminiIdentityFingerprintBlockedHeader(key) {
			return fmt.Errorf("custom header %s is managed by the system", key)
		}
		if containsHeaderLineBreak(value) {
			return fmt.Errorf("custom header %s must not contain line breaks", key)
		}
	}
	return nil
}

func containsHeaderLineBreak(value string) bool {
	return strings.ContainsAny(value, "\r\n")
}

func isIdentityFingerprintBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "connection", "chatgpt-account-id",
		"user-agent", "version", "session_id", "session-id", "originator", "openai-beta", "x-codex-beta-features":
		return true
	default:
		return false
	}
}

func isClaudeIdentityFingerprintBlockedHeader(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(key, "x-stainless-") {
		return true
	}
	switch key {
	case "x-api-key", "anthropic-beta", "anthropic-version", "anthropic-dangerous-direct-browser-access",
		"x-app", "x-client-request-id", "x-claude-code-session-id":
		return true
	default:
		return false
	}
}

func isGeminiIdentityFingerprintBlockedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "x-goog-api-client", "client-metadata":
		return true
	default:
		return false
	}
}

func isHTTPHeaderToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

package management

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func normalizeOAuthRequestScopedErrors(entries map[string][]config.RequestScopedErrorRule) map[string][]config.RequestScopedErrorRule {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]config.RequestScopedErrorRule, len(entries))
	for provider, rules := range entries {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" || len(rules) == 0 {
			continue
		}
		clean := make([]config.RequestScopedErrorRule, 0, len(rules))
		for _, rule := range rules {
			if rule.Status < 0 {
				continue
			}
			rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
			rule.Match = append([]string(nil), rule.Match...)
			rule.MatchRegexr = append([]string(nil), rule.MatchRegexr...)
			if len(rule.Match) == 0 && len(rule.MatchRegexr) == 0 && rule.Status == 0 {
				continue
			}
			clean = append(clean, rule)
		}
		if len(clean) > 0 {
			out[provider] = clean
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (h *Handler) GetOAuthRequestScopedErrors(c *gin.Context) {
	h.mu.Lock()
	var entries map[string][]config.RequestScopedErrorRule
	if h.cfg != nil {
		entries = normalizeOAuthRequestScopedErrors(h.cfg.OAuthRequestScopedErrors)
	}
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"oauth-request-scoped-errors": entries})
}

func (h *Handler) PutOAuthRequestScopedErrors(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	var entries map[string][]config.RequestScopedErrorRule
	if err = json.Unmarshal(data, &entries); err != nil {
		var wrapper struct {
			Items map[string][]config.RequestScopedErrorRule `json:"items"`
		}
		if errItems := json.Unmarshal(data, &wrapper); errItems != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		entries = wrapper.Items
	}
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	h.cfg.OAuthRequestScopedErrors = normalizeOAuthRequestScopedErrors(entries)
	h.mu.Unlock()
	h.persist(c)
}

func (h *Handler) PatchOAuthRequestScopedErrors(c *gin.Context) {
	var body struct {
		Provider *string                         `json:"provider"`
		Channel  *string                         `json:"channel"`
		Rules    []config.RequestScopedErrorRule `json:"rules"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	channel := ""
	if body.Channel != nil {
		channel = *body.Channel
	} else if body.Provider != nil {
		channel = *body.Provider
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	entries := normalizeOAuthRequestScopedErrors(h.cfg.OAuthRequestScopedErrors)
	if len(body.Rules) == 0 {
		if _, ok := entries[channel]; !ok {
			h.mu.Unlock()
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		delete(entries, channel)
	} else {
		normalized := normalizeOAuthRequestScopedErrors(map[string][]config.RequestScopedErrorRule{channel: body.Rules})
		if normalized == nil {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rules"})
			return
		}
		if entries == nil {
			entries = make(map[string][]config.RequestScopedErrorRule)
		}
		entries[channel] = normalized[channel]
	}
	h.cfg.OAuthRequestScopedErrors = normalizeOAuthRequestScopedErrors(entries)
	h.mu.Unlock()
	h.persist(c)
}

func (h *Handler) DeleteOAuthRequestScopedErrors(c *gin.Context) {
	channel := strings.ToLower(strings.TrimSpace(c.Query("channel")))
	if channel == "" {
		channel = strings.ToLower(strings.TrimSpace(c.Query("provider")))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing channel"})
		return
	}
	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	entries := normalizeOAuthRequestScopedErrors(h.cfg.OAuthRequestScopedErrors)
	if _, ok := entries[channel]; !ok {
		h.mu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	delete(entries, channel)
	h.cfg.OAuthRequestScopedErrors = normalizeOAuthRequestScopedErrors(entries)
	h.mu.Unlock()
	h.persist(c)
}

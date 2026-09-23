package management

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

var (
	openCodeGoUsageAPIURL   = "https://opencode.ai/zen/go/v1/usage"
	clineUsageAPIBaseURL    = "https://api.cline.bot"
	ollamaCloudSettingsURL  = "https://ollama.com/settings"
	openCodeGoNumberPattern = `(-?\d+(?:\.\d+)?)`
	ollamaCloudUsagePattern = regexp.MustCompile(`(?i)(Session|Weekly)\s+usage\s+` + openCodeGoNumberPattern + `%\s+used\s+Resets\s+in\s+([^\.]+)`)
	openCodeGoTagPattern    = regexp.MustCompile(`(?s)<[^>]+>`)
	openCodeGoSpacePattern  = regexp.MustCompile(`\s+`)
)

type openCodeGoUsageItem struct {
	Type       string  `json:"type"`
	Label      string  `json:"label"`
	Percentage float64 `json:"percentage"`
	ResetsIn   string  `json:"resets_in"`
}

type openCodeGoUsageRequest struct {
	Index      *int    `json:"index"`
	APIKey     string  `json:"api-key"`
	Name       string  `json:"name"`
	AuthCookie string  `json:"auth-cookie"`
	ProxyID    string  `json:"proxy-id"`
	ProxyURL   string  `json:"proxy-url"`
	TimeoutSec float64 `json:"timeout_sec"`
}

// openCodeGoUsageResponse mirrors the fields this handler reads from the
// published Go usage endpoint. Each window arrives already reduced to a
// percentage and an absolute reset instant, so nothing here has to reconstruct
// limits from raw dollar counters.
type openCodeGoUsageResponse struct {
	Usage struct {
		Rolling *openCodeGoUsageWindow `json:"rolling"`
		Weekly  *openCodeGoUsageWindow `json:"weekly"`
		Monthly *openCodeGoUsageWindow `json:"monthly"`
	} `json:"usage"`
}

type openCodeGoUsageWindow struct {
	Status   string   `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

// openCodeGoAPIError mirrors the endpoint's error envelope. The type field is
// what separates a key that is not valid (AuthError) from a workspace that
// simply never subscribed to Go (EntitlementError) — a distinction the previous
// dashboard scrape could not make, because both rendered as a page without
// usage figures.
type openCodeGoAPIError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type clineUsageResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Limits []struct {
			Type        string  `json:"type"`
			PercentUsed float64 `json:"percentUsed"`
			ResetsAt    string  `json:"resetsAt"`
		} `json:"limits"`
	} `json:"data"`
}

// QueryOpenCodeGoUsage reports OpenCode Go rolling windows for one credential.
//
// Like the Command Code check, this needs no dashboard cookie: the published
// usage endpoint authenticates with the same API key that serves inference, so
// usage keeps working without an operator pasting a browser session that later
// expires. It replaces an HTML scrape of /workspace/{id}/go, which broke once
// the console moved the figures behind a server function and localised the
// labels it used to print.
func (h *Handler) QueryOpenCodeGoUsage(c *gin.Context) {
	var body openCodeGoUsageRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	runtimeHandler := h.providerUsageHandler(c)
	entry := runtimeHandler.findOpenCodeGoEntry(body)
	apiKey := strings.TrimSpace(body.APIKey)
	proxyID := strings.TrimSpace(body.ProxyID)
	proxyURL := strings.TrimSpace(body.ProxyURL)
	if entry != nil {
		if apiKey == "" {
			apiKey = strings.TrimSpace(entry.APIKey)
		}
		if proxyID == "" {
			proxyID = strings.TrimSpace(entry.ProxyID)
		}
		if proxyURL == "" {
			proxyURL = strings.TrimSpace(entry.ProxyURL)
		}
	}
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api-key is required"})
		return
	}

	items, err := runtimeHandler.fetchOpenCodeGoUsage(c.Request.Context(), apiKey, proxyID, proxyURL, resolveUsageTimeout(body.TimeoutSec))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": items})
}

// QueryClineUsage fetches ClinePass usage limits from the dashboard API.
func (h *Handler) QueryClineUsage(c *gin.Context) {
	var body openCodeGoUsageRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	runtimeHandler := h.providerUsageHandler(c)
	entry := runtimeHandler.findClineEntry(body)
	authCookie := strings.TrimSpace(body.AuthCookie)
	proxyID := strings.TrimSpace(body.ProxyID)
	proxyURL := strings.TrimSpace(body.ProxyURL)
	if entry != nil {
		if authCookie == "" {
			authCookie = strings.TrimSpace(entry.AuthCookie)
		}
		if proxyID == "" {
			proxyID = strings.TrimSpace(entry.ProxyID)
		}
		if proxyURL == "" {
			proxyURL = strings.TrimSpace(entry.ProxyURL)
		}
	}
	authCookie = normalizeDashboardCookie(authCookie)
	if authCookie == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth-cookie is required"})
		return
	}

	items, err := runtimeHandler.fetchClineUsage(c.Request.Context(), authCookie, proxyID, proxyURL, resolveUsageTimeout(body.TimeoutSec))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": items})
}

// QueryOllamaCloudUsage fetches Ollama Cloud usage from the settings page.
func (h *Handler) QueryOllamaCloudUsage(c *gin.Context) {
	var body openCodeGoUsageRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	runtimeHandler := h.providerUsageHandler(c)
	entry := runtimeHandler.findOllamaCloudEntry(body)
	authCookie := strings.TrimSpace(body.AuthCookie)
	proxyID := strings.TrimSpace(body.ProxyID)
	proxyURL := strings.TrimSpace(body.ProxyURL)
	if entry != nil {
		if authCookie == "" {
			authCookie = strings.TrimSpace(entry.AuthCookie)
		}
		if proxyID == "" {
			proxyID = strings.TrimSpace(entry.ProxyID)
		}
		if proxyURL == "" {
			proxyURL = strings.TrimSpace(entry.ProxyURL)
		}
	}
	authCookie = normalizeDashboardCookie(authCookie)
	if authCookie == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth-cookie is required"})
		return
	}

	items, err := runtimeHandler.fetchOllamaCloudUsage(c.Request.Context(), authCookie, proxyID, proxyURL, resolveUsageTimeout(body.TimeoutSec))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": items})
}

func (h *Handler) providerUsageHandler(c *gin.Context) *Handler {
	if h == nil {
		return &Handler{}
	}
	cfg := usage.BuildTenantRuntimeConfig(h.cfg, effectiveTenantID(c))
	return &Handler{cfg: &cfg}
}

func (h *Handler) findOpenCodeGoEntry(body openCodeGoUsageRequest) *config.OpenCodeGoKey {
	if h == nil || h.cfg == nil {
		return nil
	}
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.OpenCodeGoKey) {
		return &h.cfg.OpenCodeGoKey[*body.Index]
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey != "" {
		for i := range h.cfg.OpenCodeGoKey {
			if strings.TrimSpace(h.cfg.OpenCodeGoKey[i].APIKey) == apiKey {
				return &h.cfg.OpenCodeGoKey[i]
			}
		}
	}
	name := strings.TrimSpace(body.Name)
	if name != "" {
		for i := range h.cfg.OpenCodeGoKey {
			if strings.TrimSpace(h.cfg.OpenCodeGoKey[i].Name) == name {
				return &h.cfg.OpenCodeGoKey[i]
			}
		}
	}
	return nil
}

func (h *Handler) findClineEntry(body openCodeGoUsageRequest) *config.ClineKey {
	if h == nil || h.cfg == nil {
		return nil
	}
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.ClineKey) {
		return &h.cfg.ClineKey[*body.Index]
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey != "" {
		for i := range h.cfg.ClineKey {
			if strings.TrimSpace(h.cfg.ClineKey[i].APIKey) == apiKey {
				return &h.cfg.ClineKey[i]
			}
		}
	}
	name := strings.TrimSpace(body.Name)
	if name != "" {
		for i := range h.cfg.ClineKey {
			if strings.TrimSpace(h.cfg.ClineKey[i].Name) == name {
				return &h.cfg.ClineKey[i]
			}
		}
	}
	return nil
}

func (h *Handler) findOllamaCloudEntry(body openCodeGoUsageRequest) *config.OllamaCloudKey {
	if h == nil || h.cfg == nil {
		return nil
	}
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.OllamaCloudKey) {
		return &h.cfg.OllamaCloudKey[*body.Index]
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey != "" {
		for i := range h.cfg.OllamaCloudKey {
			if strings.TrimSpace(h.cfg.OllamaCloudKey[i].APIKey) == apiKey {
				return &h.cfg.OllamaCloudKey[i]
			}
		}
	}
	name := strings.TrimSpace(body.Name)
	if name != "" {
		for i := range h.cfg.OllamaCloudKey {
			if strings.TrimSpace(h.cfg.OllamaCloudKey[i].Name) == name {
				return &h.cfg.OllamaCloudKey[i]
			}
		}
	}
	return nil
}

func (h *Handler) fetchOpenCodeGoUsage(ctx context.Context, apiKey, proxyID, proxyURL string, timeout time.Duration) ([]openCodeGoUsageItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openCodeGoUsageAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CliRelay OpenCode Go usage checker)")

	resp, err := h.usageHTTPClient(timeout, proxyID, proxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		return nil, openCodeGoUsageError(openCodeGoStatusError(body, "This OpenCode account has no Go subscription, so it reports no usage limits"))
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, openCodeGoUsageError(openCodeGoStatusError(body, "OpenCode Go API key is invalid or expired"))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openCodeGoUsageError("OpenCode Go usage API returned HTTP " + resp.Status)
	}

	var payload openCodeGoUsageResponse
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := parseOpenCodeGoUsage(payload)
	if len(items) == 0 {
		return nil, openCodeGoUsageError("OpenCode Go reported no usage windows for this account")
	}
	return items, nil
}

// openCodeGoStatusError prefers the upstream message over our own wording: the
// endpoint states the reason precisely ("OpenCode Go subscription required."),
// and a relayed reason ages better than one this code guesses from a status
// code alone.
func openCodeGoStatusError(body []byte, fallback string) string {
	var payload openCodeGoAPIError
	if err := json.Unmarshal(body, &payload); err == nil {
		if message := strings.TrimSpace(payload.Error.Message); message != "" {
			return "OpenCode Go usage API: " + message
		}
	}
	return fallback
}

func (h *Handler) fetchClineUsage(ctx context.Context, authCookie, proxyID, proxyURL string, timeout time.Duration) ([]openCodeGoUsageItem, error) {
	reqURL := strings.TrimRight(clineUsageAPIBaseURL, "/") + "/api/v1/users/me/plan/usage-limits"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", authCookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://app.cline.bot")
	req.Header.Set("Referer", "https://app.cline.bot/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CliRelay Cline usage checker)")

	resp, err := h.usageHTTPClient(timeout, proxyID, proxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, openCodeGoUsageError("Cline dashboard auth cookie is invalid or expired")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openCodeGoUsageError("Cline usage API returned HTTP " + resp.Status)
	}

	var payload clineUsageResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := parseClineUsageLimits(payload)
	if len(items) == 0 {
		return nil, openCodeGoUsageError("Cline usage data was not found")
	}
	return items, nil
}

func (h *Handler) fetchOllamaCloudUsage(ctx context.Context, authCookie, proxyID, proxyURL string, timeout time.Duration) ([]openCodeGoUsageItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaCloudSettingsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", authCookie)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CliRelay Ollama usage checker)")

	resp, err := h.usageHTTPClient(timeout, proxyID, proxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, openCodeGoUsageError("Ollama dashboard auth cookie is invalid or expired")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openCodeGoUsageError("Ollama settings page returned HTTP " + resp.Status)
	}
	items := parseOllamaCloudUsageHTML(string(body))
	if len(items) == 0 {
		text := strings.ToLower(stripOpenCodeGoHTML(string(body)))
		if strings.Contains(text, "sign in") || strings.Contains(text, "log in") {
			return nil, openCodeGoUsageError("Ollama dashboard auth cookie is invalid or expired")
		}
		return nil, openCodeGoUsageError("Ollama usage data was not found on the settings page")
	}
	return items, nil
}

func (h *Handler) usageHTTPClient(timeout time.Duration, proxyID, proxyURL string) *http.Client {
	client := util.NewHTTPClient(timeout)
	if h != nil && h.cfg != nil {
		if resolved := strings.TrimSpace(h.cfg.ResolveProxyURL(proxyID, proxyURL)); resolved != "" {
			if transport := util.BuildProxyTransport(resolved, h.cfg.PreferIPv4); transport != nil {
				client.Transport = transport
			}
		}
	}
	return client
}

type openCodeGoUsageError string

func (e openCodeGoUsageError) Error() string { return string(e) }

func normalizeDashboardCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "cookie:") {
		raw = strings.TrimSpace(raw[len("cookie:"):])
	}
	return raw
}

func resolveUsageTimeout(timeoutSec float64) time.Duration {
	timeout := 20 * time.Second
	if timeoutSec > 0 {
		timeout = time.Duration(timeoutSec * float64(time.Second))
		if timeout < 3*time.Second {
			timeout = 3 * time.Second
		}
		if timeout > 60*time.Second {
			timeout = 60 * time.Second
		}
	}
	return timeout
}

func parseOpenCodeGoUsage(payload openCodeGoUsageResponse) []openCodeGoUsageItem {
	return parseOpenCodeGoUsageAt(payload, time.Now())
}

// parseOpenCodeGoUsageAt is parseOpenCodeGoUsage with the reference instant
// passed in, so the reset countdown a test asserts does not depend on when the
// test happens to run.
func parseOpenCodeGoUsageAt(payload openCodeGoUsageResponse, now time.Time) []openCodeGoUsageItem {
	items := make([]openCodeGoUsageItem, 0, 3)
	for _, window := range []struct {
		usageType string
		label     string
		data      *openCodeGoUsageWindow
	}{
		{"rolling", "Rolling", payload.Usage.Rolling},
		{"weekly", "Weekly", payload.Usage.Weekly},
		{"monthly", "Monthly", payload.Usage.Monthly},
	} {
		// A window without a percentage carries nothing worth showing, and
		// rendering the zero value would read as "plenty left" on a plan that
		// may have none.
		if window.data == nil || window.data.Percent == nil {
			continue
		}
		items = append(items, openCodeGoUsageItem{
			Type:       window.usageType,
			Label:      window.label,
			Percentage: clampUsagePercentage(*window.data.Percent),
			ResetsIn:   formatResetAtFrom(window.data.ResetsAt, now),
		})
	}
	return items
}

func parseClineUsageLimits(payload clineUsageResponse) []openCodeGoUsageItem {
	items := make([]openCodeGoUsageItem, 0, len(payload.Data.Limits))
	for _, limit := range payload.Data.Limits {
		usageType := strings.ToLower(strings.TrimSpace(limit.Type))
		if usageType == "" {
			continue
		}
		items = append(items, openCodeGoUsageItem{
			Type:       usageType,
			Label:      labelClineUsageType(usageType),
			Percentage: clampUsagePercentage(limit.PercentUsed),
			ResetsIn:   formatResetAt(limit.ResetsAt),
		})
	}
	return items
}

func labelClineUsageType(usageType string) string {
	switch usageType {
	case "five_hour":
		return "5-Hour"
	case "weekly":
		return "Weekly"
	case "monthly":
		return "Monthly"
	default:
		return strings.ReplaceAll(usageType, "_", " ")
	}
}

func parseOllamaCloudUsageHTML(body string) []openCodeGoUsageItem {
	text := stripOpenCodeGoHTML(body)
	matches := ollamaCloudUsagePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	items := make([]openCodeGoUsageItem, 0, len(matches))
	for _, match := range matches {
		if len(match) != 4 {
			continue
		}
		percentage, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			continue
		}
		label := strings.TrimSpace(match[1])
		items = append(items, openCodeGoUsageItem{
			Type:       strings.ToLower(label),
			Label:      label,
			Percentage: clampUsagePercentage(percentage),
			ResetsIn:   strings.TrimSpace(match[3]),
		})
	}
	return items
}

func clampUsagePercentage(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func formatResetAt(raw string) string {
	return formatResetAtFrom(raw, time.Now())
}

// formatResetAtFrom is formatResetAt with the reference instant passed in.
// Reading the clock inside the formatter makes the result depend on when it is
// called, which is untestable at the boundaries: two calls a microsecond apart
// can land on either side of a rounding step and render "2 hours" and "1 hour
// 59 minutes" for the same input.
func formatResetAtFrom(raw string, now time.Time) string {
	resetAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	seconds := int64(math.Round(resetAt.Sub(now).Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	return formatOpenCodeGoResetIn(seconds)
}

func formatOpenCodeGoResetIn(seconds int64) string {
	duration := time.Duration(seconds) * time.Second
	days := int(duration / (24 * time.Hour))
	duration -= time.Duration(days) * 24 * time.Hour
	hours := int(duration / time.Hour)
	duration -= time.Duration(hours) * time.Hour
	minutes := int(duration / time.Minute)
	if days > 0 {
		if hours > 0 {
			return formatOpenCodeGoDurationPart(days, "day") + " " + formatOpenCodeGoDurationPart(hours, "hour")
		}
		return formatOpenCodeGoDurationPart(days, "day")
	}
	if hours > 0 {
		if minutes > 0 {
			return formatOpenCodeGoDurationPart(hours, "hour") + " " + formatOpenCodeGoDurationPart(minutes, "minute")
		}
		return formatOpenCodeGoDurationPart(hours, "hour")
	}
	if minutes > 0 {
		return formatOpenCodeGoDurationPart(minutes, "minute")
	}
	return formatOpenCodeGoDurationPart(int(seconds), "second")
}

func formatOpenCodeGoDurationPart(value int, unit string) string {
	suffix := unit
	if value != 1 {
		suffix += "s"
	}
	return strconv.Itoa(value) + " " + suffix
}

func stripOpenCodeGoHTML(body string) string {
	text := openCodeGoTagPattern.ReplaceAllString(body, " ")
	text = html.UnescapeString(text)
	text = openCodeGoSpacePattern.ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

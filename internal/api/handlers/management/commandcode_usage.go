package management

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Command Code plan usage.
//
// Unlike the Cline and Ollama Cloud checks in opencode_go_usage.go, and like the
// OpenCode Go one, this needs no dashboard cookie: the credits endpoint
// authenticates with the same API key that serves inference, so usage keeps
// working without the operator pasting a browser cookie that later expires.
//
// The endpoint is not part of Command Code's published API reference — it was
// observed on the live gateway. Treat every failure as "usage unavailable" rather
// than "channel broken": a key that cannot read credits still serves requests
// perfectly well, so nothing here may be turned into a credential error.

var commandCodeCreditsURL = "https://api.commandcode.ai/alpha/billing/credits"

// commandCodeCreditsResponse mirrors only the fields this handler reads.
// cap/used are plan-window counters; resetAt has been observed in both seconds
// and milliseconds, so it is normalized on read rather than trusted.
type commandCodeCreditsResponse struct {
	WindowLimits struct {
		FiveHour *commandCodeWindow `json:"fiveHour"`
		Weekly   *commandCodeWindow `json:"weekly"`
	} `json:"windowLimits"`
	Credits *struct {
		MonthlyCredits   *float64 `json:"monthlyCredits"`
		PurchasedCredits *float64 `json:"purchasedCredits"`
		FreeCredits      *float64 `json:"freeCredits"`
	} `json:"credits"`
}

type commandCodeWindow struct {
	Cap     *float64 `json:"cap"`
	Used    *float64 `json:"used"`
	ResetAt *float64 `json:"resetAt"`
}

// QueryCommandCodeUsage reports Command Code plan windows for one credential.
func (h *Handler) QueryCommandCodeUsage(c *gin.Context) {
	var body openCodeGoUsageRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	runtimeHandler := h.providerUsageHandler(c)
	entry := runtimeHandler.findCommandCodeEntry(body)
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

	items, err := runtimeHandler.fetchCommandCodeUsage(c.Request.Context(), apiKey, proxyID, proxyURL, resolveUsageTimeout(body.TimeoutSec))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": items})
}

func (h *Handler) findCommandCodeEntry(body openCodeGoUsageRequest) *config.CommandCodeKey {
	if h == nil || h.cfg == nil {
		return nil
	}
	if body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.CommandCodeKey) {
		return &h.cfg.CommandCodeKey[*body.Index]
	}
	apiKey := strings.TrimSpace(body.APIKey)
	if apiKey != "" {
		for i := range h.cfg.CommandCodeKey {
			if strings.TrimSpace(h.cfg.CommandCodeKey[i].APIKey) == apiKey {
				return &h.cfg.CommandCodeKey[i]
			}
		}
	}
	name := strings.TrimSpace(body.Name)
	if name != "" {
		for i := range h.cfg.CommandCodeKey {
			if strings.TrimSpace(h.cfg.CommandCodeKey[i].Name) == name {
				return &h.cfg.CommandCodeKey[i]
			}
		}
	}
	return nil
}

func (h *Handler) fetchCommandCodeUsage(ctx context.Context, apiKey, proxyID, proxyURL string, timeout time.Duration) ([]openCodeGoUsageItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, commandCodeCreditsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CliRelay Command Code usage checker)")

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
		return nil, openCodeGoUsageError("Command Code API key is invalid or lacks billing access")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openCodeGoUsageError("Command Code credits API returned HTTP " + resp.Status)
	}

	var payload commandCodeCreditsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	items := parseCommandCodeUsage(payload)
	if len(items) == 0 {
		return nil, openCodeGoUsageError("Command Code reported no plan windows for this account")
	}
	return items, nil
}

func parseCommandCodeUsage(payload commandCodeCreditsResponse) []openCodeGoUsageItem {
	items := make([]openCodeGoUsageItem, 0, 2)
	if item, ok := commandCodeWindowItem("five_hour", "5-Hour", payload.WindowLimits.FiveHour); ok {
		items = append(items, item)
	}
	if item, ok := commandCodeWindowItem("weekly", "Weekly", payload.WindowLimits.Weekly); ok {
		items = append(items, item)
	}
	return items
}

func commandCodeWindowItem(usageType, label string, window *commandCodeWindow) (openCodeGoUsageItem, bool) {
	// A window without a positive cap carries no ratio worth showing; reporting
	// it as 0% would read as "plenty left" on a plan that may have none.
	if window == nil || window.Cap == nil || window.Used == nil || *window.Cap <= 0 {
		return openCodeGoUsageItem{}, false
	}
	percentage := clampUsagePercentage(*window.Used / *window.Cap * 100)
	return openCodeGoUsageItem{
		Type:       usageType,
		Label:      label,
		Percentage: percentage,
		ResetsIn:   commandCodeResetIn(window.ResetAt),
	}, true
}

// commandCodeResetIn accepts either a seconds or a milliseconds epoch. Values
// past the year 2001 in milliseconds exceed 1e12, which is the only reliable way
// to tell the two apart without a documented unit.
func commandCodeResetIn(resetAt *float64) string {
	return commandCodeResetInAt(resetAt, time.Now())
}

// commandCodeResetInAt is commandCodeResetIn with the reference instant passed
// in. Reading the clock inside the formatter makes the result depend on when it
// is called, which is untestable at the boundaries: two calls a microsecond
// apart can land on either side of a rounding step and render "2 hours" and
// "1 hour 59 minutes" for the same input.
func commandCodeResetInAt(resetAt *float64, now time.Time) string {
	if resetAt == nil || *resetAt <= 0 || math.IsNaN(*resetAt) || math.IsInf(*resetAt, 0) {
		return ""
	}
	milliseconds := *resetAt
	if milliseconds <= 1e12 {
		milliseconds *= 1000
	}
	seconds := int64(math.Round(time.UnixMilli(int64(milliseconds)).Sub(now).Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	return formatOpenCodeGoResetIn(seconds)
}

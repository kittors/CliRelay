package aiaccountstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	codexUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

func probeCodex(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	body, err := doAuthGET(ctx, svc, auth, codexUsageURL, map[string]string{
		"Content-Type":   "application/json",
		"User-Agent":     config.DefaultCodexFingerprintUserAgent,
		"Originator":     "Codex Desktop",
		"OpenAI-Beta":    "codex-1",
		"Accept":         "application/json",
		"Sec-Fetch-Site": "none",
		"Sec-Fetch-Mode": "no-cors",
		"Sec-Fetch-Dest": "empty",
	}, func(req *http.Request) {
		if accountID := codexAccountID(auth); accountID != "" {
			req.Header.Set("Chatgpt-Account-Id", accountID)
		}
	})
	if err != nil {
		return ProbeResult{}, err
	}
	_ = svc.ReconcileCodexWhamUsagePlan(ctx, auth, parseURL(codexUsageURL), 200, body)

	root := gjson.ParseBytes(body)
	result := ProbeResult{
		PlanType: strings.ToLower(strings.TrimSpace(firstJSONResult(root, "plan_type", "planType").String())),
		Quotas:   parseCodexWhamQuotas(body),
	}
	credits := firstJSONResult(root, "rate_limit_reset_credits", "rateLimitResetCredits")
	if count := firstJSONResult(credits, "available_count", "availableCount"); count.Exists() {
		v := count.Int()
		result.ResetCreditCount = &v
		if v > 0 {
			if expBody, expErr := doAuthGET(ctx, svc, auth, codexResetCreditsURL, map[string]string{
				"Content-Type":   "application/json",
				"User-Agent":     config.DefaultCodexFingerprintUserAgent,
				"Originator":     "Codex Desktop",
				"OpenAI-Beta":    "codex-1",
				"Accept":         "application/json",
				"Sec-Fetch-Site": "none",
				"Sec-Fetch-Mode": "no-cors",
				"Sec-Fetch-Dest": "empty",
			}, func(req *http.Request) {
				if accountID := codexAccountID(auth); accountID != "" {
					req.Header.Set("Chatgpt-Account-Id", accountID)
				}
			}); expErr == nil {
				result.ResetCreditExpirations = parseCodexResetExpirations(expBody)
			}
		}
	}
	return result, nil
}

func parseCodexWhamQuotas(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	items := make([]usage.QuotaWindowDTO, 0, 12)
	appendLimit := func(limit gjson.Result, prefix string, includeNonStandard bool) {
		if !limit.Exists() {
			return
		}
		windows := codexRateLimitWindows(limit)
		var fiveHour, weekly gjson.Result
		for _, window := range windows {
			switch codexWindowSeconds(window) {
			case 18000:
				if !fiveHour.Exists() {
					fiveHour = window
				}
			case 604800:
				if !weekly.Exists() {
					weekly = window
				}
			}
		}
		if fiveHour.Exists() {
			label := "m_quota.code_5h"
			if prefix == "review" {
				label = "m_quota.review_5h"
			}
			items = append(items, codexWindowDTO(limit, fiveHour, prefix+"_5h", label, 18000))
		}
		if weekly.Exists() {
			label := "m_quota.code_weekly"
			if prefix == "review" {
				label = "m_quota.review_weekly"
			}
			items = append(items, codexWindowDTO(limit, weekly, prefix+"_week", label, 604800))
		}
		if !includeNonStandard {
			return
		}
		for _, window := range windows {
			seconds := codexWindowSeconds(window)
			if seconds <= 0 || seconds == 18000 || seconds == 604800 {
				continue
			}
			label := "m_quota.code_subscription"
			if prefix == "review" {
				label = "m_quota.review_subscription"
			}
			key := fmt.Sprintf("%s_subscription_%d", prefix, seconds)
			items = append(items, codexWindowDTO(limit, window, key, label, seconds))
		}
	}

	rateLimit := firstJSONResult(root, "rate_limit", "rateLimit")
	appendLimit(rateLimit, "code", true)
	appendLimit(firstJSONResult(root, "code_review_rate_limit", "codeReviewRateLimit"), "review", true)

	additional := firstJSONResult(root, "additional_rate_limits", "additionalRateLimits")
	if additional.IsArray() {
		additional.ForEach(func(_, entry gjson.Result) bool {
			limit := firstJSONResult(entry, "rate_limit", "rateLimit")
			if !limit.Exists() {
				return true
			}
			name := strings.TrimSpace(firstJSONResult(entry, "limit_name", "limitName").String())
			if name == "" {
				name = "Additional Codex quota"
			}
			keyPart := strings.TrimSpace(firstJSONResult(entry, "metered_feature", "meteredFeature").String())
			if keyPart == "" {
				if strings.EqualFold(name, "gpt-5.3-codex-spark") {
					keyPart = "codex_bengalfox"
				} else {
					keyPart = normalizeQuotaKeyPart(name)
				}
			} else {
				keyPart = normalizeQuotaKeyPart(keyPart)
			}
			if keyPart == "" {
				keyPart = "additional"
			}
			for _, window := range codexRateLimitWindows(limit) {
				seconds := codexWindowSeconds(window)
				suffix, label := "", ""
				switch seconds {
				case 18000:
					suffix, label = "5h", name+": 5h"
				case 604800:
					suffix, label = "week", name+": Weekly"
				default:
					continue
				}
				items = append(items, codexWindowDTO(limit, window, "additional:"+keyPart+":"+suffix, label, seconds))
			}
			return true
		})
	}
	return items
}

func codexRateLimitWindows(limit gjson.Result) []gjson.Result {
	windows := make([]gjson.Result, 0, 2)
	for _, paths := range [][2]string{{"primary_window", "primaryWindow"}, {"secondary_window", "secondaryWindow"}} {
		if window := firstJSONResult(limit, paths[0], paths[1]); window.Exists() {
			windows = append(windows, window)
		}
	}
	return windows
}

func codexWindowSeconds(window gjson.Result) int64 {
	return firstJSONResult(window, "limit_window_seconds", "limitWindowSeconds").Int()
}

func codexWindowDTO(limit, window gjson.Result, key, label string, seconds int64) usage.QuotaWindowDTO {
	dto := usage.QuotaWindowDTO{QuotaKey: key, QuotaLabel: label, WindowSeconds: seconds}
	if used := firstJSONResult(window, "used_percent", "usedPercent"); used.Exists() {
		remaining := 100 - clampPct(used.Float())
		dto.Percent = &remaining
	} else {
		limitReached := firstJSONResult(limit, "limit_reached", "limitReached")
		allowed := limit.Get("allowed")
		if (limitReached.Exists() && limitReached.Bool()) || (allowed.Exists() && !allowed.Bool()) {
			remaining := 0.0
			dto.Percent = &remaining
		}
	}
	if resetAt := firstJSONResult(window, "reset_at", "resetAt"); resetAt.Exists() && resetAt.Int() > 0 {
		t := time.Unix(resetAt.Int(), 0).UTC()
		dto.ResetAt = &t
	} else if after := firstJSONResult(window, "reset_after_seconds", "resetAfterSeconds"); after.Exists() && after.Float() > 0 {
		t := time.Now().UTC().Add(time.Duration(after.Float() * float64(time.Second)))
		dto.ResetAt = &t
	}
	return dto
}

func normalizeQuotaKeyPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		isAlphaNum := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if isAlphaNum {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if b.Len() > 0 && !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func parseCodexResetExpirations(body []byte) []string {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	var walk func(any, int)
	walk = func(value any, depth int) {
		if depth > 6 || value == nil {
			return
		}
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				walk(item, depth+1)
			}
		case map[string]any:
			for _, key := range []string{"expires_at", "expiresAt"} {
				if raw, ok := typed[key].(string); ok {
					value := strings.TrimSpace(raw)
					if value != "" {
						if _, exists := seen[value]; !exists {
							seen[value] = struct{}{}
							values = append(values, value)
						}
					}
				}
			}
			for _, child := range typed {
				walk(child, depth+1)
			}
		}
	}
	walk(payload, 0)
	sort.SliceStable(values, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339, values[i])
		right, rightErr := time.Parse(time.RFC3339, values[j])
		if leftErr != nil || rightErr != nil {
			return values[i] < values[j]
		}
		return left.Before(right)
	})
	return values
}

func codexAccountID(auth *coreauth.Auth) string {
	if direct := authString(auth, "account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"); direct != "" {
		return direct
	}
	idToken := authString(auth, "id_token", "idToken")
	if idToken == "" {
		return ""
	}
	claims, err := codexauth.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
}

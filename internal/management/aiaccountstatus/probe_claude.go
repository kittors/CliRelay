package aiaccountstatus

import (
	"context"
	"strings"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const (
	claudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
)

func claudeProbeHeaders() map[string]string {
	return map[string]string{
		"Accept":         "application/json, text/plain, */*",
		"Content-Type":   "application/json",
		"User-Agent":     "claude-code/2.1.7",
		"anthropic-beta": "oauth-2025-04-20",
	}
}

func probeClaude(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	body, err := doAuthGET(ctx, svc, auth, claudeUsageURL, claudeProbeHeaders(), nil)
	if err != nil {
		return ProbeResult{}, err
	}
	result := ProbeResult{Quotas: parseClaudeUsage(body)}
	// Best effort: usage data stays valid even when the profile call fails.
	if profileBody, profileErr := doAuthGET(ctx, svc, auth, claudeProfileURL, claudeProbeHeaders(), nil); profileErr == nil {
		result.PlanType = parseClaudePlanType(profileBody)
	}
	return result, nil
}

func parseClaudeUsage(body []byte) []usage.QuotaWindowDTO {
	keys := []struct {
		paths  []string
		key    string
		label  string
		window int64
	}{
		{[]string{"five_hour"}, "five_hour", "claude_quota.five_hour", 18000},
		{[]string{"seven_day"}, "seven_day", "claude_quota.seven_day", 604800},
		{[]string{"seven_day_oauth_apps"}, "seven_day_oauth_apps", "claude_quota.seven_day_oauth_apps", 604800},
		{[]string{"seven_day_opus"}, "seven_day_opus", "claude_quota.seven_day_opus", 604800},
		{[]string{"seven_day_sonnet"}, "seven_day_sonnet", "claude_quota.seven_day_sonnet", 604800},
		{[]string{"seven_day_cowork", "seven_day_routines", "seven_day_claude_routines", "claude_routines", "routines"}, "seven_day_cowork", "claude_quota.seven_day_cowork", 604800},
		{[]string{"iguana_necktie"}, "iguana_necktie", "claude_quota.iguana_necktie", 0},
	}
	out := make([]usage.QuotaWindowDTO, 0, len(keys)+1)
	root := gjson.ParseBytes(body)
	for _, k := range keys {
		var win gjson.Result
		for _, path := range k.paths {
			if candidate := root.Get(path); candidate.Exists() && candidate.IsObject() {
				win = candidate
				break
			}
		}
		if !win.Exists() {
			continue
		}
		dto := usage.QuotaWindowDTO{QuotaKey: k.key, QuotaLabel: k.label, WindowSeconds: k.window}
		if utilPct := win.Get("utilization"); utilPct.Exists() {
			remaining := 100 - clampPct(utilPct.Float())
			dto.Percent = &remaining
		}
		if reset := firstJSONResult(win, "resets_at", "resetsAt"); reset.Exists() {
			dto.ResetAt = parseFlexibleTime(reset)
		}
		if dto.Percent != nil || dto.ResetAt != nil {
			out = append(out, dto)
		}
	}
	out = appendClaudeScopedLimits(out, root)
	extra := root.Get("extra_usage")
	if extra.Exists() && extra.Get("is_enabled").Bool() {
		if utilization := extra.Get("utilization"); utilization.Exists() {
			remaining := 100 - clampPct(utilization.Float())
			used := strings.TrimSpace(extra.Get("used_credits").String())
			limit := strings.TrimSpace(extra.Get("monthly_limit").String())
			meta := ""
			if used != "" && limit != "" {
				meta = used + " / " + limit + " credits"
			}
			out = append(out, usage.QuotaWindowDTO{
				QuotaKey: "extra_usage", QuotaLabel: "claude_quota.extra_usage_label", Percent: &remaining, Meta: meta,
			})
		}
	}
	return out
}

// appendClaudeScopedLimits maps the newer `limits[]` shape (kind "weekly_scoped")
// that superseded the flat `seven_day_<model>` fields for model-scoped weekly windows.
func appendClaudeScopedLimits(out []usage.QuotaWindowDTO, root gjson.Result) []usage.QuotaWindowDTO {
	limits := root.Get("limits")
	if !limits.IsArray() {
		return out
	}
	seen := make(map[string]bool, len(out))
	for _, dto := range out {
		seen[dto.QuotaKey] = true
	}
	for _, entry := range limits.Array() {
		if entry.Get("kind").String() != "weekly_scoped" || entry.Get("group").String() != "weekly" {
			continue
		}
		percent := entry.Get("percent")
		if !percent.Exists() {
			continue
		}
		model := entry.Get("scope.model")
		name := strings.TrimSpace(firstJSONResult(model, "display_name", "displayName").String())
		id := strings.TrimSpace(model.Get("id").String())
		if name == "" {
			name = id
		}
		slug := claudeModelSlug(id)
		if slug == "" {
			slug = claudeModelSlug(name)
		}
		// "All models" scopes duplicate the main seven_day window.
		if slug == "" || slug == "all-models" || strings.HasSuffix(slug, "-all-models") || claudeModelSlug(name) == "all-models" {
			continue
		}
		// Flat seven_day_opus / seven_day_sonnet already cover these models when present.
		if (seen["seven_day_opus"] && strings.Contains(slug, "opus")) ||
			(seen["seven_day_sonnet"] && strings.Contains(slug, "sonnet")) {
			continue
		}
		key := "weekly_scoped_" + slug
		if seen[key] {
			continue
		}
		remaining := 100 - clampPct(percent.Float())
		dto := usage.QuotaWindowDTO{
			QuotaKey:      key,
			QuotaLabel:    "claude_quota.model_weekly::" + name,
			WindowSeconds: 604800,
			Percent:       &remaining,
		}
		if reset := firstJSONResult(entry, "resets_at", "resetsAt"); reset.Exists() {
			dto.ResetAt = parseFlexibleTime(reset)
		}
		seen[key] = true
		out = append(out, dto)
	}
	return out
}

func claudeModelSlug(value string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// parseClaudePlanType maps /api/oauth/profile onto membership plan keys the
// dashboard renders (max_20x / max_5x / max / pro / team / enterprise / free).
func parseClaudePlanType(body []byte) string {
	root := gjson.ParseBytes(body)
	orgType := strings.ToLower(strings.TrimSpace(firstJSONResult(root,
		"organization.organization_type", "organization.organizationType").String()))
	if strings.Contains(orgType, "enterprise") {
		return "enterprise"
	}
	if strings.Contains(orgType, "team") {
		return "team"
	}
	tier := strings.ToLower(strings.TrimSpace(firstJSONResult(root,
		"organization.rate_limit_tier", "organization.rateLimitTier").String()))
	switch {
	case strings.Contains(tier, "max_20x"):
		return "max_20x"
	case strings.Contains(tier, "max_5x"):
		return "max_5x"
	case strings.Contains(tier, "max"):
		return "max"
	case strings.Contains(tier, "pro"):
		return "pro"
	}
	switch {
	case strings.Contains(orgType, "max"):
		return "max"
	case strings.Contains(orgType, "pro"):
		return "pro"
	case strings.Contains(orgType, "free"):
		return "free"
	}
	if root.Get("account.has_claude_max").Bool() {
		return "max"
	}
	if root.Get("account.has_claude_pro").Bool() {
		return "pro"
	}
	return ""
}

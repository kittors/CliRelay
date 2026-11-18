package aiaccountstatus

import (
	"context"
	"fmt"
	"math"
	"strings"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v8/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const kiroQuotaURL = "https://codewhisperer.us-east-1.amazonaws.com"

func probeKiro(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	body, err := doAuthPOST(ctx, svc, auth, kiroQuotaURL, map[string]string{
		"Content-Type": "application/x-amz-json-1.0",
		"x-amz-target": "AmazonCodeWhispererService.GetUsageLimits",
	}, `{"origin":"AI_EDITOR","resourceType":"AGENTIC_REQUEST"}`)
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Quotas: parseKiroQuota(body)}, nil
}

func parseKiroQuota(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	out := make([]usage.QuotaWindowDTO, 0, 3)
	if subscription := strings.TrimSpace(root.Get("subscriptionInfo.subscriptionTitle").String()); subscription != "" {
		out = append(out, usage.QuotaWindowDTO{
			QuotaKey: "subscription", QuotaLabel: "m_quota.subscription", Meta: subscription, Value: subscription,
		})
	}
	usage0 := root.Get("usageBreakdownList.0")
	if !usage0.Exists() {
		return out
	}
	if limit, used := usage0.Get("usageLimitWithPrecision"), usage0.Get("currentUsageWithPrecision"); limit.Exists() && used.Exists() {
		remaining := 0.0
		if limit.Float() > 0 {
			remaining = math.Round(clampPct(((limit.Float() - used.Float()) / limit.Float()) * 100))
		}
		dto := usage.QuotaWindowDTO{
			QuotaKey: "base_quota", QuotaLabel: "m_quota.base_quota", Percent: &remaining,
			Meta: fmt.Sprintf("used %.0f / limit %.0f", used.Float(), limit.Float()),
		}
		reset := usage0.Get("nextDateReset")
		if !reset.Exists() {
			reset = root.Get("nextDateReset")
		}
		dto.ResetAt = parseFlexibleTime(reset)
		out = append(out, dto)
	}
	trial := usage0.Get("freeTrialInfo")
	if trial.Exists() {
		limit, used := trial.Get("usageLimitWithPrecision"), trial.Get("currentUsageWithPrecision")
		if limit.Exists() && used.Exists() {
			remaining := 0.0
			if limit.Float() > 0 {
				remaining = math.Round(clampPct(((limit.Float() - used.Float()) / limit.Float()) * 100))
			}
			status := strings.TrimSpace(trial.Get("freeTrialStatus").String())
			if status == "" {
				status = "trial"
			}
			dto := usage.QuotaWindowDTO{
				QuotaKey: "trial_quota", QuotaLabel: "m_quota.trial_quota", Percent: &remaining,
				Meta:    fmt.Sprintf("%s · used %.0f / limit %.0f", status, used.Float(), limit.Float()),
				ResetAt: parseFlexibleTime(trial.Get("freeTrialExpiry")),
			}
			out = append(out, dto)
		}
	}
	return out
}

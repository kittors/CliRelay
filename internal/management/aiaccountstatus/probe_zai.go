package aiaccountstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// OpenAI-compatible names are user-defined. Only the official coding URLs
// select a monitor; other providers and pay-as-you-go URLs stay unsupported.
func zaiCodingQuotaURL(auth *coreauth.Auth) string {
	return map[string]string{
		"https://api.z.ai/api/coding/paas/v4":         "https://api.z.ai/api/monitor/usage/quota/limit",
		"https://open.bigmodel.cn/api/coding/paas/v4": "https://open.bigmodel.cn/api/monitor/usage/quota/limit",
	}[strings.TrimRight(authString(auth, "base_url"), "/")]
}

func probeZAICoding(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, endpoint string) (ProbeResult, error) {
	if endpoint == "" {
		return ProbeResult{Unsupported: true, UnsupportedReason: "provider " + normalizeProvider(auth.Provider) + " has no server-side status probe"}, nil
	}
	body, err := doAuthGET(ctx, svc, auth, endpoint, map[string]string{"Accept": "application/json"}, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	return parseZAICodingQuota(body)
}

type zaiCodingLimit struct {
	Type          string   `json:"type"`
	Unit          *float64 `json:"unit"`
	Number        *float64 `json:"number"`
	Percentage    *float64 `json:"percentage"`
	NextResetTime int64    `json:"nextResetTime"`
}

func parseZAICodingQuota(body []byte) (ProbeResult, error) {
	var response struct {
		Code    int  `json:"code"`
		Success bool `json:"success"`
		Data    struct {
			Level  string           `json:"level"`
			Limits []zaiCodingLimit `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return ProbeResult{}, fmt.Errorf("invalid Z.ai quota response")
	}
	if response.Code != 200 || !response.Success {
		return ProbeResult{}, fmt.Errorf("Z.ai quota request unsuccessful")
	}
	quotas := make([]usage.QuotaWindowDTO, 0, 2)
	for _, limit := range response.Data.Limits {
		quota, err := limit.quotaWindow()
		if err != nil {
			return ProbeResult{}, err
		}
		if quota != nil {
			quotas = append(quotas, *quota)
		}
	}
	if len(quotas) == 0 {
		return ProbeResult{}, fmt.Errorf("no supported Z.ai quota windows")
	}
	return ProbeResult{PlanType: strings.TrimSpace(response.Data.Level), Quotas: quotas}, nil
}

func (limit zaiCodingLimit) quotaWindow() (*usage.QuotaWindowDTO, error) {
	switch limit.Type {
	case "CREDIT_LIMIT", "TOKENS_LIMIT":
	default:
		return nil, nil
	}
	seconds := limit.windowSeconds()
	if seconds == 0 {
		return nil, nil
	}
	remaining, err := zaiRemainingPercent(limit.Percentage)
	if err != nil {
		return nil, err
	}
	quota := &usage.QuotaWindowDTO{
		QuotaKey: "code_5h", QuotaLabel: "m_quota.code_5h", WindowSeconds: seconds, Percent: &remaining,
	}
	if seconds == 604800 {
		quota.QuotaKey, quota.QuotaLabel = "code_week", "m_quota.code_weekly"
	}
	if limit.NextResetTime > 0 {
		t := time.UnixMilli(limit.NextResetTime).UTC()
		quota.ResetAt = &t
	}
	return quota, nil
}

func (limit zaiCodingLimit) windowSeconds() int64 {
	// Legacy token-limit responses exposed only the five-hour window.
	if limit.Type == "TOKENS_LIMIT" && limit.Unit == nil && limit.Number == nil {
		return 18000
	}
	if limit.Unit == nil || limit.Number == nil {
		return 0
	}
	return map[[2]float64]int64{
		{3, 5}: 18000,
		{6, 1}: 604800,
	}[[2]float64{*limit.Unit, *limit.Number}]
}

func zaiRemainingPercent(used *float64) (float64, error) {
	if used == nil || *used < 0 || *used > 100 {
		return 0, fmt.Errorf("invalid Z.ai quota percentage")
	}
	// CliRelay's shared DTO is remaining percent; Z.ai reports used percent.
	return 100 - *used, nil
}

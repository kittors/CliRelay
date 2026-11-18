package aiaccountstatus

import (
	"context"
	"math"
	"strings"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v8/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const kimiUsageURL = "https://api.kimi.com/coding/v1/usages"

func probeKimi(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	body, err := doAuthGET(ctx, svc, auth, kimiUsageURL, nil, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Quotas: parseKimiUsage(body)}, nil
}

func parseKimiUsage(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	source := root
	if !root.Get("usage").Exists() && !root.Get("limits").Exists() {
		usages := root.Get("usages")
		if !usages.IsArray() {
			return nil
		}
		source = gjson.Result{}
		usages.ForEach(func(_, entry gjson.Result) bool {
			if !source.Exists() {
				source = entry
			}
			if strings.EqualFold(strings.TrimSpace(entry.Get("scope").String()), "FEATURE_CODING") {
				source = entry
				return false
			}
			return true
		})
		if !source.Exists() {
			return nil
		}
	}

	var fiveHourDetail, weeklyDetail gjson.Result
	limits := source.Get("limits")
	if limits.IsArray() {
		limits.ForEach(func(_, limit gjson.Result) bool {
			switch kimiWindowMinutes(limit.Get("window")) {
			case 300:
				if !fiveHourDetail.Exists() {
					fiveHourDetail = limit.Get("detail")
				}
			case 7 * 24 * 60:
				if !weeklyDetail.Exists() {
					weeklyDetail = limit.Get("detail")
				}
			}
			return true
		})
	}
	if detail := source.Get("usage"); detail.Exists() {
		weeklyDetail = detail
	} else if detail := source.Get("detail"); detail.Exists() {
		weeklyDetail = detail
	}

	out := make([]usage.QuotaWindowDTO, 0, 2)
	if dto, ok := kimiDetailDTO("code_5h", "m_quota.code_5h", 18000, fiveHourDetail); ok {
		out = append(out, dto)
	}
	if dto, ok := kimiDetailDTO("code_week", "m_quota.code_weekly", 604800, weeklyDetail); ok {
		out = append(out, dto)
	}
	return out
}

func kimiWindowMinutes(window gjson.Result) int64 {
	if !window.Exists() {
		return 0
	}
	duration := window.Get("duration").Float()
	if duration <= 0 {
		return 0
	}
	unit := strings.ToUpper(strings.TrimSpace(firstJSONResult(window, "timeUnit", "time_unit").String()))
	switch unit {
	case "", "TIME_UNIT_MINUTE":
		return int64(duration)
	case "TIME_UNIT_HOUR":
		return int64(duration * 60)
	case "TIME_UNIT_DAY":
		return int64(duration * 24 * 60)
	case "TIME_UNIT_WEEK":
		return int64(duration * 7 * 24 * 60)
	default:
		return 0
	}
}

func kimiDetailDTO(key, label string, windowSeconds int64, detail gjson.Result) (usage.QuotaWindowDTO, bool) {
	if !detail.Exists() {
		return usage.QuotaWindowDTO{}, false
	}
	dto := usage.QuotaWindowDTO{QuotaKey: key, QuotaLabel: label, WindowSeconds: windowSeconds}
	limit := detail.Get("limit")
	if limit.Exists() {
		limitValue := limit.Float()
		if limitValue <= 0 {
			remaining := 0.0
			dto.Percent = &remaining
		} else if remainingAmount := detail.Get("remaining"); remainingAmount.Exists() {
			remaining := math.Round(clampPct((remainingAmount.Float() / limitValue) * 100))
			dto.Percent = &remaining
		} else if used := detail.Get("used"); used.Exists() {
			remaining := math.Round(clampPct(((limitValue - used.Float()) / limitValue) * 100))
			dto.Percent = &remaining
		}
	}
	dto.ResetAt = parseFlexibleTime(firstJSONResult(detail, "resetTime", "reset_time"))
	return dto, dto.Percent != nil || dto.ResetAt != nil
}

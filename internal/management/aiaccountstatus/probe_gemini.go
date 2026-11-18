package aiaccountstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v8/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const geminiCLIQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"

func probeGeminiCLI(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	projectID := firstNonEmpty(
		authString(auth, "project_id", "projectId", "gemini_virtual_project"),
		metadataNestedString(auth, "installed", "project_id", "projectId"),
	)
	if projectID == "" {
		return ProbeResult{Unsupported: true, UnsupportedReason: "missing_project_id"}, nil
	}
	payload, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return ProbeResult{}, fmt.Errorf("encode gemini project: %w", err)
	}
	body, err := doAuthPOST(ctx, svc, auth, geminiCLIQuotaURL, map[string]string{
		"Content-Type": "application/json",
	}, string(payload))
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Quotas: parseGeminiCLIQuota(body)}, nil
}

type geminiQuotaBucket struct {
	modelID           string
	tokenType         string
	remainingFraction *float64
	remainingAmount   *float64
	resetAt           *time.Time
}

type geminiQuotaGroup struct {
	id               string
	label            string
	preferredModelID string
	modelIDs         []string
}

var geminiQuotaGroups = []geminiQuotaGroup{
	{id: "gemini-2.5-pro", label: "Gemini 2.5 Pro", preferredModelID: "gemini-2.5-pro", modelIDs: []string{"gemini-2.5-pro", "gemini-2.5-pro-preview"}},
	{id: "gemini-2.5-flash", label: "Gemini 2.5 Flash", preferredModelID: "gemini-2.5-flash", modelIDs: []string{"gemini-2.5-flash", "gemini-2.5-flash-preview"}},
	{id: "gemini-2.5-flash-lite", label: "Gemini 2.5 Flash Lite", preferredModelID: "gemini-2.5-flash-lite", modelIDs: []string{"gemini-2.5-flash-lite"}},
	{id: "gemini-1.5-pro", label: "Gemini 1.5 Pro", preferredModelID: "gemini-1.5-pro", modelIDs: []string{"gemini-1.5-pro", "gemini-1.5-pro-latest"}},
	{id: "gemini-1.5-flash", label: "Gemini 1.5 Flash", preferredModelID: "gemini-1.5-flash", modelIDs: []string{"gemini-1.5-flash", "gemini-1.5-flash-latest"}},
}

func parseGeminiCLIQuota(body []byte) []usage.QuotaWindowDTO {
	parsed := make([]geminiQuotaBucket, 0)
	gjson.GetBytes(body, "buckets").ForEach(func(_, raw gjson.Result) bool {
		modelID := strings.TrimSpace(firstJSONResult(raw, "modelId", "model_id").String())
		if strings.HasPrefix(modelID, "projects/") {
			parts := strings.SplitN(modelID, "/", 3)
			if len(parts) == 3 {
				modelID = strings.TrimSpace(parts[2])
			}
		}
		if modelID == "" || strings.HasPrefix(modelID, "gemini-2.0-flash") {
			return true
		}
		bucket := geminiQuotaBucket{
			modelID:   modelID,
			tokenType: strings.TrimSpace(firstJSONResult(raw, "tokenType", "token_type").String()),
			resetAt:   parseFlexibleTime(firstJSONResult(raw, "resetTime", "reset_time")),
		}
		if fraction := firstJSONResult(raw, "remainingFraction", "remaining_fraction"); fraction.Exists() {
			value := quotaFraction(fraction)
			if value != nil {
				bucket.remainingFraction = value
			}
		}
		if amount := firstJSONResult(raw, "remainingAmount", "remaining_amount"); amount.Exists() {
			value := amount.Float()
			bucket.remainingAmount = &value
		}
		if bucket.remainingFraction == nil {
			if bucket.remainingAmount != nil && *bucket.remainingAmount <= 0 || bucket.remainingAmount == nil && bucket.resetAt != nil {
				zero := 0.0
				bucket.remainingFraction = &zero
			}
		}
		parsed = append(parsed, bucket)
		return true
	})

	type groupedBucket struct {
		id, label, tokenType, preferredModelID string
		preferred, fallback                    *geminiQuotaBucket
		order                                  int
	}
	groups := make(map[string]*groupedBucket)
	for i := range parsed {
		bucket := &parsed[i]
		groupID, label, preferred, order := bucket.modelID, bucket.modelID, "", len(geminiQuotaGroups)+1
		for groupIndex, definition := range geminiQuotaGroups {
			for _, modelID := range definition.modelIDs {
				if bucket.modelID == modelID {
					groupID, label, preferred, order = definition.id, definition.label, definition.preferredModelID, groupIndex
					break
				}
			}
			if groupID == definition.id {
				break
			}
		}
		key := groupID + "|" + bucket.tokenType
		group := groups[key]
		if group == nil {
			group = &groupedBucket{id: groupID, label: label, tokenType: bucket.tokenType, preferredModelID: preferred, order: order}
			groups[key] = group
		}
		if group.fallback == nil || group.fallback.remainingFraction == nil && bucket.remainingFraction != nil {
			group.fallback = bucket
		}
		if bucket.modelID == group.preferredModelID {
			group.preferred = bucket
		}
	}
	ordered := make([]*groupedBucket, 0, len(groups))
	for _, group := range groups {
		ordered = append(ordered, group)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].order != ordered[j].order {
			return ordered[i].order < ordered[j].order
		}
		if ordered[i].id != ordered[j].id {
			return ordered[i].id < ordered[j].id
		}
		return ordered[i].tokenType < ordered[j].tokenType
	})

	out := make([]usage.QuotaWindowDTO, 0, len(ordered))
	for _, group := range ordered {
		bucket := group.preferred
		if bucket == nil {
			bucket = group.fallback
		}
		if bucket == nil {
			continue
		}
		dto := usage.QuotaWindowDTO{QuotaKey: "model:" + group.id, QuotaLabel: group.label, ResetAt: bucket.resetAt}
		if group.tokenType != "" {
			dto.QuotaKey += ":" + normalizeQuotaKeyPart(group.tokenType)
		}
		if bucket.remainingFraction != nil {
			remaining := math.Round(clampPct(*bucket.remainingFraction * 100))
			dto.Percent = &remaining
		}
		meta := make([]string, 0, 2)
		if group.tokenType != "" {
			meta = append(meta, "tokenType="+group.tokenType)
		}
		if bucket.remainingAmount != nil {
			meta = append(meta, fmt.Sprintf("%.0f tokens", *bucket.remainingAmount))
		}
		dto.Meta = strings.Join(meta, " · ")
		out = append(out, dto)
	}
	return out
}

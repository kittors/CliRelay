package aiaccountstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v8/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// antigravityProbeUserAgent is the shared Antigravity client identity. The
// probe and the request executor must present the same client to CloudCode:
// when they drifted apart the upstream answered them differently, and the probe
// — on the older version — was served a coarser quota view than the account
// actually had.
const antigravityProbeUserAgent = antigravityauth.ClientUserAgent

// antigravityHosts is ordered sandbox-first. The sandbox host is the one that
// stays reachable when the production host is shedding load with 429s.
var antigravityHosts = []string{
	"https://daily-cloudcode-pa.sandbox.googleapis.com",
	"https://daily-cloudcode-pa.googleapis.com",
	"https://cloudcode-pa.googleapis.com",
}

const (
	antigravityQuotaSummaryPath = "/v1internal:retrieveUserQuotaSummary"
	antigravityModelsPath       = "/v1internal:fetchAvailableModels"
	antigravityLoadPath         = "/v1internal:loadCodeAssist"
)

// antigravityFallbackProject is the shared project the upstream accepts when an
// account has no project of its own. It is a last resort, not a default: asking
// for quota under a project that is not yours reports that project's remaining
// fraction, which is how exhausted accounts come back as 100% available.
const antigravityFallbackProject = "bamboo-precept-lgxtn"

const (
	antigravityWindowFiveHour = int64(5 * 60 * 60)
	antigravityWindowWeek     = int64(7 * 24 * 60 * 60)
)

func probeAntigravity(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) (ProbeResult, error) {
	account := resolveAntigravityAccount(ctx, svc, auth)

	quotas, err := fetchAntigravityQuotas(ctx, svc, auth, account.projectID)
	if err != nil {
		return ProbeResult{}, err
	}
	return ProbeResult{Quotas: quotas, PlanType: account.planType}, nil
}

// antigravityAccount is the per-account context the quota endpoints need.
type antigravityAccount struct {
	projectID string
	planType  string
}

// antigravityAccountCache keeps loadCodeAssist results for a short while. The
// endpoint is the same one the request path hits on every cold token, and it
// rate-limits well before the quota endpoints do.
var antigravityAccountCache sync.Map // auth key -> antigravityAccountCacheEntry

type antigravityAccountCacheEntry struct {
	account   antigravityAccount
	expiresAt time.Time
}

const antigravityAccountCacheTTL = 10 * time.Minute

func antigravityAuthKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id
	}
	return strings.TrimSpace(auth.Index)
}

// resolveAntigravityAccount answers "which project and which plan is this
// account on". The stored metadata is trusted first, then loadCodeAssist, and
// only then the shared fallback project.
func resolveAntigravityAccount(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) antigravityAccount {
	stored := firstNonEmpty(
		metadataString(auth, "project_id", "projectId"),
		metadataNestedString(auth, "installed", "project_id", "projectId"),
		metadataNestedString(auth, "web", "project_id", "projectId"),
	)

	cacheKey := antigravityAuthKey(auth)
	if cacheKey != "" {
		if raw, ok := antigravityAccountCache.Load(cacheKey); ok {
			if entry, okEntry := raw.(antigravityAccountCacheEntry); okEntry && time.Now().Before(entry.expiresAt) {
				if entry.account.projectID == "" {
					entry.account.projectID = stored
				}
				return entry.account
			}
		}
	}

	account := loadAntigravityCodeAssist(ctx, svc, auth)
	if account.projectID == "" {
		account.projectID = stored
	}
	if cacheKey != "" && (account.projectID != "" || account.planType != "") {
		antigravityAccountCache.Store(cacheKey, antigravityAccountCacheEntry{
			account:   account,
			expiresAt: time.Now().Add(antigravityAccountCacheTTL),
		})
	}
	return account
}

func loadAntigravityCodeAssist(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth) antigravityAccount {
	payload, err := json.Marshal(map[string]any{
		"metadata": map[string]string{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	})
	if err != nil {
		return antigravityAccount{}
	}
	for _, host := range antigravityHosts {
		body, err := doAuthPOST(ctx, svc, auth, host+antigravityLoadPath, antigravityHeaders(), string(payload))
		if err != nil {
			continue
		}
		return parseAntigravityCodeAssist(body)
	}
	return antigravityAccount{}
}

func parseAntigravityCodeAssist(body []byte) antigravityAccount {
	root := gjson.ParseBytes(body)
	account := antigravityAccount{}

	project := root.Get("cloudaicompanionProject")
	if project.IsObject() {
		account.projectID = strings.TrimSpace(project.Get("id").String())
	} else {
		account.projectID = strings.TrimSpace(project.String())
	}

	account.planType = parseAntigravityPlanType(root)
	return account
}

// parseAntigravityPlanType walks the same ladder the upstream's own client does.
// A paid tier wins outright; otherwise the current tier counts only when the
// account is not marked ineligible, and an ineligible account falls back to the
// default allowed tier flagged as restricted.
func parseAntigravityPlanType(root gjson.Result) string {
	tierName := func(tier gjson.Result) string {
		if !tier.Exists() {
			return ""
		}
		if name := strings.TrimSpace(tier.Get("name").String()); name != "" {
			return name
		}
		return strings.TrimSpace(tier.Get("id").String())
	}

	if paid := tierName(root.Get("paidTier")); paid != "" {
		return paid
	}

	ineligible := root.Get("ineligibleTiers")
	isIneligible := ineligible.IsArray() && len(ineligible.Array()) > 0
	if !isIneligible {
		if current := tierName(root.Get("currentTier")); current != "" {
			return current
		}
		return ""
	}

	var restricted string
	root.Get("allowedTiers").ForEach(func(_, tier gjson.Result) bool {
		if !tier.Get("isDefault").Bool() {
			return true
		}
		if name := tierName(tier); name != "" {
			restricted = name + " (Restricted)"
			return false
		}
		return true
	})
	return restricted
}

func antigravityHeaders() map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"User-Agent":   antigravityProbeUserAgent,
	}
}

// fetchAntigravityQuotas prefers the grouped summary, which reports the account's
// real quota buckets — weekly and 5h, per model family — as the upstream itself
// groups them. fetchAvailableModels is the fallback: it only carries the 5h
// window and leaves the grouping to us.
func fetchAntigravityQuotas(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, projectID string) ([]usage.QuotaWindowDTO, error) {
	if quotas := fetchAntigravityQuotaSummary(ctx, svc, auth, projectID); len(quotas) > 0 {
		return quotas, nil
	}
	return fetchAntigravityModelQuotas(ctx, svc, auth, projectID)
}

func fetchAntigravityQuotaSummary(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, projectID string) []usage.QuotaWindowDTO {
	for _, host := range antigravityHosts {
		body, status, err := antigravityPost(ctx, svc, auth, host+antigravityQuotaSummaryPath, projectID)
		if err != nil {
			// A 4xx other than 429 means every host will answer the same way.
			if status >= 400 && status < 500 && status != 429 {
				return nil
			}
			continue
		}
		if quotas := parseAntigravityQuotaSummary(body); len(quotas) > 0 {
			return quotas
		}
	}
	return nil
}

func fetchAntigravityModelQuotas(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, projectID string) ([]usage.QuotaWindowDTO, error) {
	var lastErr error
	for _, host := range antigravityHosts {
		body, _, err := antigravityPost(ctx, svc, auth, host+antigravityModelsPath, projectID)
		if err != nil {
			lastErr = err
			continue
		}
		quotas := parseAntigravityModels(body)
		if len(quotas) == 0 {
			lastErr = fmt.Errorf("no_model_quota")
			continue
		}
		return quotas, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("request_failed")
	}
	return nil, lastErr
}

// antigravityPost sends the project-scoped request and, on 403, retries once
// without the project field. A project the account cannot read is rejected
// outright, and the accountless form is what the upstream falls back to.
func antigravityPost(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, url, projectID string) ([]byte, int, error) {
	payloads := make([]string, 0, 2)
	if strings.TrimSpace(projectID) != "" {
		if encoded, err := json.Marshal(map[string]string{"project": projectID}); err == nil {
			payloads = append(payloads, string(encoded))
		}
	}
	payloads = append(payloads, "{}")

	var lastBody []byte
	var lastStatus int
	var lastErr error
	for _, payload := range payloads {
		body, status, err := doAuthPOSTStatus(ctx, svc, auth, url, antigravityHeaders(), payload)
		if err == nil {
			return body, status, nil
		}
		lastBody, lastStatus, lastErr = body, status, err
		if status != 403 {
			break
		}
	}
	return lastBody, lastStatus, lastErr
}

// parseAntigravityQuotaSummary reads retrieveUserQuotaSummary. Every label and
// every grouping here comes from the upstream payload: the account's real
// buckets are `gemini-weekly`, `gemini-5h`, `3p-weekly`, `3p-5h`, and inventing
// our own model-family split on top of them is what made one bucket render as
// several rows showing the same number.
func parseAntigravityQuotaSummary(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	groups := firstJSONResult(root, "groups", "quotaGroups", "quota_groups")
	if !groups.IsArray() {
		return nil
	}

	out := make([]usage.QuotaWindowDTO, 0, 8)
	seen := make(map[string]struct{}, 8)
	groups.ForEach(func(groupIndex, group gjson.Result) bool {
		groupName := strings.TrimSpace(firstJSONResult(group, "displayName", "display_name").String())
		groupDescription := strings.TrimSpace(firstJSONResult(group, "description").String())
		buckets := firstJSONResult(group, "buckets", "quotaBuckets", "quota_buckets")
		if !buckets.IsArray() {
			return true
		}
		buckets.ForEach(func(bucketIndex, bucket gjson.Result) bool {
			dto, ok := antigravityBucketDTO(bucket, groupName, groupDescription, groupIndex.Int(), bucketIndex.Int())
			if !ok {
				return true
			}
			if _, duplicate := seen[dto.QuotaKey]; duplicate {
				return true
			}
			seen[dto.QuotaKey] = struct{}{}
			out = append(out, dto)
			return true
		})
		return true
	})
	return out
}

func antigravityBucketDTO(bucket gjson.Result, groupName, groupDescription string, groupIndex, bucketIndex int64) (usage.QuotaWindowDTO, bool) {
	fraction := firstJSONResult(bucket, "remainingFraction", "remaining_fraction", "remaining")
	resetAt := parseFlexibleTime(firstJSONResult(bucket, "resetTime", "reset_time"))
	if !fraction.Exists() && resetAt == nil {
		return usage.QuotaWindowDTO{}, false
	}

	bucketID := strings.TrimSpace(firstJSONResult(bucket, "bucketId", "bucket_id", "id").String())
	window := strings.TrimSpace(bucket.Get("window").String())
	windowSeconds := antigravityWindowSeconds(window, bucketID)

	key := normalizeQuotaKeyPart(bucketID)
	if key == "" {
		key = fmt.Sprintf("g%d_b%d", groupIndex, bucketIndex)
		if suffix := normalizeQuotaKeyPart(window); suffix != "" {
			key = fmt.Sprintf("g%d_%s", groupIndex, suffix)
		}
	}

	dto := usage.QuotaWindowDTO{
		QuotaKey:      "antigravity:" + key,
		QuotaLabel:    antigravityGroupLabel(groupName, bucketID, window),
		Meta:          antigravityBucketMeta(bucket, groupDescription),
		ResetAt:       resetAt,
		WindowSeconds: windowSeconds,
	}
	if fraction.Exists() {
		if value := quotaFraction(fraction); value != nil {
			percent := math.Round(clampPct(*value * 100))
			dto.Percent = &percent
		}
	}
	return dto, true
}

// antigravityGroupLabel names the row after the model group only.
//
// The upstream also names the bucket ("Weekly Limit Remaining"), and pairing
// that with the group produced labels like
// "Gemini Models · Weekly Limit Remaining" — too long for a card row, and
// redundant once the window is rendered from WindowSeconds. The card composes
// "<group> · <localised window>" itself, so the group name is all this has to
// carry, and it still comes from the upstream rather than a table here.
func antigravityGroupLabel(groupName, bucketID, window string) string {
	if groupName != "" {
		return groupName
	}
	if bucketID != "" {
		return bucketID
	}
	return window
}

// antigravityBucketMeta carries the upstream's own explanation of the group —
// typically which models draw on it. The card keeps it out of the row and shows
// it on hover, so a long sentence cannot crowd out the numbers.
func antigravityBucketMeta(bucket gjson.Result, groupDescription string) string {
	if description := strings.TrimSpace(firstJSONResult(bucket, "description").String()); description != "" {
		if groupDescription != "" && !strings.EqualFold(description, groupDescription) {
			return groupDescription + " · " + description
		}
		return description
	}
	return groupDescription
}

// antigravityWindowSeconds maps the upstream's window token onto a duration.
// Unknown tokens return 0 rather than guessing, so a window we cannot classify
// is still reported — it just does not claim to be the weekly cycle.
func antigravityWindowSeconds(window, bucketID string) int64 {
	normalize := func(value string) string {
		v := strings.ToLower(strings.TrimSpace(value))
		v = strings.ReplaceAll(v, "_", "")
		v = strings.ReplaceAll(v, "-", "")
		return strings.ReplaceAll(v, " ", "")
	}
	for _, candidate := range []string{normalize(window), normalize(bucketID)} {
		if candidate == "" {
			continue
		}
		switch {
		case strings.Contains(candidate, "week"):
			return antigravityWindowWeek
		case strings.Contains(candidate, "month"):
			return 30 * 24 * 60 * 60
		case strings.Contains(candidate, "day") || strings.Contains(candidate, "daily"):
			return 24 * 60 * 60
		}
		if seconds := parseAntigravityHourToken(candidate); seconds > 0 {
			return seconds
		}
	}
	return 0
}

// parseAntigravityHourToken reads the digits immediately preceding an `h`.
// Every `h` is considered, not just the first: a bucket id like `gemini-flash-5h`
// carries an `h` inside "flash" that no digits precede, and stopping there would
// classify a 5h bucket as having no window at all.
func parseAntigravityHourToken(value string) int64 {
	for i := 0; i < len(value); i++ {
		if value[i] != 'h' {
			continue
		}
		start := i
		for start > 0 && value[start-1] >= '0' && value[start-1] <= '9' {
			start--
		}
		if start == i {
			continue
		}
		hours, err := strconv.ParseInt(value[start:i], 10, 64)
		if err != nil || hours <= 0 || hours > 24*31 {
			continue
		}
		return hours * 60 * 60
	}
	return 0
}

// parseAntigravityModels is the fallback view built from fetchAvailableModels,
// used when the account cannot read the grouped summary.
//
// One row per model family, each showing a single representative model's real
// quota. Picking a representative rather than aggregating the family matters:
// on a live account gemini-2.5-pro and gemini-3.1-pro-high are both "Gemini
// Pro" yet draw on different buckets, so folding them together produced the
// minimum of two unrelated quotas — a number belonging to neither, and a reset
// countdown from whichever bucket happened to expire first.
//
// Preference order is a ranking, not an allowlist: a family with none of its
// preferred ids still shows whichever member the upstream did send, so a model
// shipped after this code cannot make a row disappear.
func parseAntigravityModels(body []byte) []usage.QuotaWindowDTO {
	root := gjson.ParseBytes(body)
	models := root.Get("models")
	if !models.IsObject() && root.IsObject() {
		models = root
	}
	if !models.IsObject() {
		return nil
	}

	type candidate struct {
		id          string
		displayName string
		percent     *float64
		resetAt     *time.Time
	}
	families := make(map[string][]candidate, 8)

	models.ForEach(func(modelID, model gjson.Result) bool {
		id := normalizeAntigravityModelID(modelID.String())
		if id == "" || isAntigravityInternalModel(id) {
			return true
		}
		info := firstJSONResult(model, "quotaInfo", "quota_info")
		fraction := firstJSONResult(info, "remainingFraction", "remaining_fraction", "remaining")
		resetAt := parseFlexibleTime(firstJSONResult(info, "resetTime", "reset_time"))
		if !fraction.Exists() && resetAt == nil {
			return true
		}
		var percent *float64
		if fraction.Exists() {
			if value := quotaFraction(fraction); value != nil {
				remaining := math.Round(clampPct(*value * 100))
				percent = &remaining
			}
		}
		family := categorizeAntigravityModel(id)
		families[family] = append(families[family], candidate{
			id:          id,
			displayName: strings.TrimSpace(firstJSONResult(model, "displayName", "display_name").String()),
			percent:     percent,
			resetAt:     resetAt,
		})
		return true
	})

	out := make([]usage.QuotaWindowDTO, 0, len(families))
	for _, family := range antigravityFamilyOrder {
		members := families[family]
		if len(members) == 0 {
			continue
		}
		sort.SliceStable(members, func(i, j int) bool {
			return antigravityPreferenceRank(family, members[i].id) < antigravityPreferenceRank(family, members[j].id)
		})
		chosen := members[0]
		label := antigravityFamilyLabel(family)
		if family == antigravityCategoryOther {
			label = chosen.displayName
			if label == "" {
				label = chosen.id
			}
		}
		out = append(out, usage.QuotaWindowDTO{
			QuotaKey:      "antigravity:" + family,
			QuotaLabel:    label,
			Percent:       chosen.percent,
			ResetAt:       chosen.resetAt,
			WindowSeconds: antigravityWindowFiveHour,
			// The row shows one model's quota; naming it lets the card explain
			// which, instead of implying the whole family was measured.
			Meta: chosen.id,
		})
	}

	// "other" collapses every unclassified model onto one row, so emit each of
	// them instead: they are unrelated models that merely failed classification.
	if extras := families[antigravityCategoryOther]; len(extras) > 1 {
		out = out[:len(out)-1]
		sort.SliceStable(extras, func(i, j int) bool { return extras[i].id < extras[j].id })
		for _, extra := range extras {
			label := extra.displayName
			if label == "" {
				label = extra.id
			}
			out = append(out, usage.QuotaWindowDTO{
				QuotaKey:      "antigravity:model_" + normalizeQuotaKeyPart(extra.id),
				QuotaLabel:    label,
				Percent:       extra.percent,
				ResetAt:       extra.resetAt,
				WindowSeconds: antigravityWindowFiveHour,
				Meta:          extra.id,
			})
		}
	}
	return out
}

const (
	antigravityCategoryGeminiPro   = "gemini_pro"
	antigravityCategoryGeminiFlash = "gemini_flash"
	antigravityCategoryGeminiImage = "gemini_image"
	antigravityCategoryClaude      = "claude"
	antigravityCategoryOther       = "other"
)

var antigravityFamilyOrder = []string{
	antigravityCategoryGeminiPro,
	antigravityCategoryGeminiFlash,
	antigravityCategoryGeminiImage,
	antigravityCategoryClaude,
	antigravityCategoryOther,
}

// antigravityPreferences ranks which member best represents its family — the
// agent-facing model an operator actually spends. Absent ids simply rank last,
// so this never decides whether a row exists.
var antigravityPreferences = map[string][]string{
	antigravityCategoryGeminiPro:   {"gemini-pro-agent", "gemini-3.1-pro-high", "gemini-3.1-pro-low"},
	antigravityCategoryGeminiFlash: {"gemini-3-flash-agent", "gemini-3-flash"},
	antigravityCategoryGeminiImage: {"gemini-3.1-flash-image", "gemini-3-pro-image"},
	antigravityCategoryClaude:      {"claude-sonnet-4-6", "claude-opus-4-6-thinking"},
}

func antigravityPreferenceRank(family, id string) int {
	for index, preferred := range antigravityPreferences[family] {
		if preferred == id {
			return index
		}
	}
	// Unpreferred members keep upstream order behind the preferred ones.
	return len(antigravityPreferences[family]) + 1
}

// categorizeAntigravityModel groups a model by the shape of its id instead of by
// membership in a list, so a model that does not exist yet still lands in the
// right family.
func categorizeAntigravityModel(id string) string {
	switch {
	case strings.HasPrefix(id, "gemini") && strings.Contains(id, "image"),
		strings.HasPrefix(id, "image"),
		strings.HasPrefix(id, "imagen"):
		return antigravityCategoryGeminiImage
	case strings.HasPrefix(id, "gemini") && strings.Contains(id, "flash"):
		return antigravityCategoryGeminiFlash
	case strings.HasPrefix(id, "gemini") && strings.Contains(id, "pro"):
		return antigravityCategoryGeminiPro
	case strings.Contains(id, "claude"),
		strings.Contains(id, "opus"),
		strings.Contains(id, "sonnet"),
		strings.Contains(id, "haiku"):
		return antigravityCategoryClaude
	default:
		return antigravityCategoryOther
	}
}

func antigravityFamilyLabel(family string) string {
	switch family {
	case antigravityCategoryGeminiPro:
		return "Gemini Pro"
	case antigravityCategoryGeminiFlash:
		return "Gemini Flash"
	case antigravityCategoryGeminiImage:
		return "Gemini Image"
	case antigravityCategoryClaude:
		return "Claude"
	default:
		return ""
	}
}

func normalizeAntigravityModelID(raw string) string {
	id := strings.ToLower(strings.TrimSpace(raw))
	return strings.TrimPrefix(id, "models/")
}

// isAntigravityInternalModel filters the upstream's non-conversational entries.
// These are matched by shape rather than by id so a newly added internal model
// does not surface as a user-visible quota row.
func isAntigravityInternalModel(id string) bool {
	return strings.HasPrefix(id, "chat_") || strings.HasPrefix(id, "tab_")
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

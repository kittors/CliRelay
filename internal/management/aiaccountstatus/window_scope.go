package aiaccountstatus

import "strings"

// Which requests a quota window limits.
//
// The probes in this package name each window after what the upstream reports,
// and scheduling has to know which of them a request for one model draws from:
// an account whose Opus allowance is spent still serves Sonnet, and the Codex
// code-review allowance says nothing about chat requests. The spend shown next
// to each window asks the same question the other way round — which recorded
// requests fed it — so both read windowModelScope. The rule lives next to the
// code that builds the keys, so a new key is classified where it is introduced.

// accountWideWindowKeys limit every model request of an account.
var accountWideWindowKeys = map[string]bool{
	// Codex and Kimi coding plans.
	"code_5h":   true,
	"code_week": true,
	// Claude subscriptions.
	"five_hour": true,
	"seven_day": true,
	// xAI: the weekly pool and the included monthly credits.
	"weekly_limit":    true,
	"monthly_credits": true,
	// Kiro credits.
	"base_quota":  true,
	"trial_quota": true,
}

// codexSparkFeatureKey is the metered feature Codex reports for the
// gpt-5.3-codex-spark allowance; its limit name is the only place the model
// appears.
const codexSparkFeatureKey = "codex_bengalfox"

// WindowAppliesToModel reports whether the window named quotaKey limits
// requests for model. An empty model asks about the account as a whole, which
// only account-wide windows answer.
//
// Windows for something other than model requests (Codex code review, Claude
// OAuth apps and routines, xAI per-product shares and the on-demand spending
// cap) apply to no model: counting them would shed or reorder traffic over an
// allowance the request does not touch.
func WindowAppliesToModel(quotaKey, model string) bool {
	accountWide, covers := windowModelScope(quotaKey)
	if accountWide {
		return true
	}
	model = normalizeScheduledModel(model)
	return model != "" && covers != nil && covers(model)
}

// windowSpendCovers returns which recorded requests count toward a window's
// spend: every request of the account (nil filter) for an account-wide window,
// the models it limits for a model-scoped one. It reports false for windows no
// model request draws on, whose spend the proxy's own traffic cannot tell.
func windowSpendCovers(quotaKey string) (func(model string) bool, bool) {
	accountWide, covers := windowModelScope(quotaKey)
	if accountWide {
		return nil, true
	}
	if covers == nil {
		return nil, false
	}
	return func(model string) bool {
		model = normalizeScheduledModel(model)
		return model != "" && covers(model)
	}, true
}

// windowModelScope classifies quotaKey. An account-wide window limits every
// request; otherwise covers accepts the (normalized) models the window limits,
// and is nil for allowances that no model request draws on.
func windowModelScope(quotaKey string) (accountWide bool, covers func(model string) bool) {
	key := strings.ToLower(strings.TrimSpace(quotaKey))
	if key == "" {
		return false, nil
	}
	if accountWideWindowKeys[key] || strings.HasPrefix(key, "code_subscription_") {
		return true, nil
	}
	switch {
	case strings.HasPrefix(key, "additional:"):
		rest := strings.TrimPrefix(key, "additional:")
		return false, func(model string) bool { return codexAdditionalCoversModel(rest, model) }
	case key == "seven_day_opus":
		return false, func(model string) bool { return strings.Contains(model, "opus") }
	case key == "seven_day_sonnet":
		return false, func(model string) bool { return strings.Contains(model, "sonnet") }
	case strings.HasPrefix(key, "weekly_scoped_"):
		// The slug comes from the scope's model id or display name ("claude-opus-4-5",
		// "opus-4-5"), either of which a dated request model id contains.
		slug := strings.TrimPrefix(key, "weekly_scoped_")
		if slug == "" {
			return false, nil
		}
		return false, func(model string) bool { return strings.Contains(claudeModelSlug(model), slug) }
	case strings.HasPrefix(key, "model:"):
		rest := strings.TrimPrefix(key, "model:")
		return false, func(model string) bool { return geminiGroupCoversModel(rest, model) }
	case strings.HasPrefix(key, "antigravity:model_"):
		id := strings.TrimPrefix(key, "antigravity:model_")
		return false, func(model string) bool { return normalizeQuotaKeyPart(model) == id }
	case strings.HasPrefix(key, "antigravity:"):
		return false, antigravityWindowCovers(strings.TrimPrefix(key, "antigravity:"))
	}
	return false, nil
}

// antigravityWindowCovers reads the two shapes of Antigravity window keys.
//
// The per-family rows (gemini_pro, claude, ...) come from the fallback model
// listing and limit their own family. The quota summary instead reports the
// account's real buckets — gemini-5h / gemini-weekly for every Gemini model and
// 3p-5h / 3p-weekly for the third-party (Claude, GPT) ones — whose keys name no
// family. Those used to match no model at all, so scheduling never weighed the
// buckets an Antigravity account actually spends.
func antigravityWindowCovers(family string) func(model string) bool {
	switch family {
	case antigravityCategoryOther:
		return nil
	case antigravityCategoryGeminiPro, antigravityCategoryGeminiFlash,
		antigravityCategoryGeminiImage, antigravityCategoryClaude:
		return func(model string) bool { return categorizeAntigravityModel(model) == family }
	}
	switch {
	case strings.HasPrefix(family, "gemini_"):
		return isAntigravityGeminiModel
	case strings.HasPrefix(family, "3p_"):
		return func(model string) bool { return !isAntigravityGeminiModel(model) }
	}
	return nil
}

// isAntigravityGeminiModel reports whether model draws on the Gemini bucket
// rather than the third-party one.
func isAntigravityGeminiModel(model string) bool {
	switch categorizeAntigravityModel(model) {
	case antigravityCategoryGeminiPro, antigravityCategoryGeminiFlash, antigravityCategoryGeminiImage:
		return true
	}
	return strings.HasPrefix(model, "gemini")
}

func normalizeScheduledModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.TrimPrefix(model, "models/")
}

// codexAdditionalCoversModel reads "<feature>:<5h|week>". The feature is either
// the metered feature Codex reports or the normalized limit name, which for
// these allowances is the model name.
func codexAdditionalCoversModel(rest, model string) bool {
	feature := rest
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		feature = rest[:i]
	}
	switch feature {
	case "", "additional":
		return false
	case codexSparkFeatureKey:
		return strings.Contains(model, "codex-spark")
	}
	return strings.Contains(normalizeQuotaKeyPart(model), feature)
}

// geminiGroupCoversModel reads "<group id>[:<token type>]". Known groups list
// their models; a bucket outside every group is keyed by its own model id.
func geminiGroupCoversModel(rest, model string) bool {
	groupID := rest
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		groupID = rest[:i]
	}
	if groupID == "" {
		return false
	}
	for _, group := range geminiQuotaGroups {
		if group.id != groupID {
			continue
		}
		for _, modelID := range group.modelIDs {
			if modelID == model {
				return true
			}
		}
		return false
	}
	return groupID == model
}

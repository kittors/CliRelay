package aiaccountstatus

import "strings"

// Which requests a quota window limits.
//
// The probes in this package name each window after what the upstream reports,
// and scheduling has to know which of them a request for one model draws from:
// an account whose Opus allowance is spent still serves Sonnet, and the Codex
// code-review allowance says nothing about chat requests. The rule lives next
// to the code that builds the keys, so a new key is classified where it is
// introduced.

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
	key := strings.ToLower(strings.TrimSpace(quotaKey))
	if key == "" {
		return false
	}
	if accountWideWindowKeys[key] || strings.HasPrefix(key, "code_subscription_") {
		return true
	}
	model = normalizeScheduledModel(model)
	if model == "" {
		return false
	}
	switch {
	case strings.HasPrefix(key, "additional:"):
		return codexAdditionalCoversModel(strings.TrimPrefix(key, "additional:"), model)
	case key == "seven_day_opus":
		return strings.Contains(model, "opus")
	case key == "seven_day_sonnet":
		return strings.Contains(model, "sonnet")
	case strings.HasPrefix(key, "weekly_scoped_"):
		// The slug comes from the scope's model id or display name ("claude-opus-4-5",
		// "opus-4-5"), either of which a dated request model id contains.
		slug := strings.TrimPrefix(key, "weekly_scoped_")
		return slug != "" && strings.Contains(claudeModelSlug(model), slug)
	case strings.HasPrefix(key, "model:"):
		return geminiGroupCoversModel(strings.TrimPrefix(key, "model:"), model)
	case strings.HasPrefix(key, "antigravity:model_"):
		return normalizeQuotaKeyPart(model) == strings.TrimPrefix(key, "antigravity:model_")
	case strings.HasPrefix(key, "antigravity:"):
		family := strings.TrimPrefix(key, "antigravity:")
		return family != antigravityCategoryOther && categorizeAntigravityModel(model) == family
	}
	return false
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

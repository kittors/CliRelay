package auth

import (
	"strings"
)

// AntigravityQuotaFamily identifies the quota pool family for Antigravity models.
type AntigravityQuotaFamily string

const (
	AntigravityFamilyGemini AntigravityQuotaFamily = "gemini"
	AntigravityFamilyClaude AntigravityQuotaFamily = "claude"
	AntigravityFamilyOther  AntigravityQuotaFamily = "other"
)

// ModelAntigravityQuotaFamily returns the quota family for an Antigravity model ID.
// Gemini models (gemini-*) share the Gemini quota pool.
// Claude and GPT models (claude-*, gpt-*) share the 3rd-party quota pool.
func ModelAntigravityQuotaFamily(model string) AntigravityQuotaFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "claude"),
		strings.Contains(m, "opus"),
		strings.Contains(m, "sonnet"),
		strings.Contains(m, "haiku"),
		strings.Contains(m, "gpt"):
		return AntigravityFamilyClaude
	case strings.HasPrefix(m, "gemini"),
		strings.Contains(m, "image"),
		strings.Contains(m, "imagen"):
		return AntigravityFamilyGemini
	default:
		return AntigravityFamilyOther
	}
}

// IsAntigravityAuth reports whether an Auth instance belongs to the Antigravity provider.
func IsAntigravityAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return true
	}
	if auth.Metadata != nil {
		if typ, _ := auth.Metadata["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "antigravity") {
			return true
		}
	}
	return false
}

// AreAntigravityModelsSameFamily reports whether two Antigravity models share the same quota pool.
func AreAntigravityModelsSameFamily(modelA, modelB string) bool {
	famA := ModelAntigravityQuotaFamily(modelA)
	famB := ModelAntigravityQuotaFamily(modelB)
	if famA == AntigravityFamilyOther || famB == AntigravityFamilyOther {
		return strings.EqualFold(strings.TrimSpace(modelA), strings.TrimSpace(modelB))
	}
	return famA == famB
}

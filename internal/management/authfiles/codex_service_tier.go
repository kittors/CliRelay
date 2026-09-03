package authfiles

import (
	"fmt"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	metadataKeyCodexServiceTier = "codex_service_tier"
)

// ValidCodexServiceTier reports whether tier is a known service_tier action/value.
// Supported values:
//   - "auto" / "" (default): inherit / standard behavior (strip non-standard service_tier)
//   - "pass": passthrough client-provided service_tier (normalize "fast" -> "priority")
//   - "priority": force fast/priority tier (always inject service_tier="priority")
//   - "flex": force flex tier (always inject service_tier="flex")
//   - "drop": completely strip service_tier from outbound request
func ValidCodexServiceTier(tier string) bool {
	switch strings.TrimSpace(strings.ToLower(tier)) {
	case "", "auto", "default", "pass", "priority", "fast", "flex", "drop":
		return true
	default:
		return false
	}
}

// NormalizeCodexServiceTier canonicalizes the configured service_tier option.
func NormalizeCodexServiceTier(tier string) string {
	switch strings.TrimSpace(strings.ToLower(tier)) {
	case "priority", "fast":
		return "priority"
	case "pass":
		return "pass"
	case "flex":
		return "flex"
	case "drop":
		return "drop"
	case "default", "auto":
		return "default"
	default:
		return ""
	}
}

// CodexServiceTierPayload returns the configured per-account service_tier option if present.
func CodexServiceTierPayload(auth *coreauth.Auth) string {
	if !isCodexOAuthAdmissionAuth(auth) || auth.Metadata == nil {
		return ""
	}
	for _, key := range []string{metadataKeyCodexServiceTier, "service_tier", "codex-service-tier"} {
		if raw, ok := auth.Metadata[key]; ok {
			if s, isStr := raw.(string); isStr {
				if norm := NormalizeCodexServiceTier(s); norm != "" {
					return norm
				}
			}
		}
	}
	return ""
}

func ensureCodexServiceTierEditable(auth *coreauth.Auth, tier string) error {
	if !isCodexOAuthAdmissionAuth(auth) {
		return fmt.Errorf("codex service tier is only supported for Codex OAuth auth files")
	}
	trimmed := strings.TrimSpace(strings.ToLower(tier))
	if trimmed != "" && !ValidCodexServiceTier(trimmed) {
		return fmt.Errorf("invalid codex service tier %q: must be empty, pass, priority (fast), flex, or drop", tier)
	}
	return nil
}

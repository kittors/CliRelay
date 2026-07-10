package identityfingerprint

import (
	"sort"
	"strings"
	"unicode"
)

const (
	ProfileKeyDefault      = "default"
	ProfileKeyCodexDesktop = "codex_desktop"
	ProfileFamilyCLI       = "cli"
	ProfileFamilyDesktop   = "desktop"
	ProfileFamilyIDE       = "ide"
	ProfileFamilyUnknown   = "unknown"
)

func DefaultProfileKey(provider Provider) string {
	if provider == ProviderCodex {
		return "codex_unknown"
	}
	return ProfileKeyDefault
}

func NormalizeAccountPolicy(provider Provider, accountKey string, policy AccountPolicy) AccountPolicy {
	policy.Provider = provider
	policy.AccountKey = strings.TrimSpace(accountKey)
	policy.ActiveProfileKey = strings.TrimSpace(policy.ActiveProfileKey)
	switch policy.Strategy {
	case AccountStrategyActiveProfile:
		if policy.ActiveProfileKey == "" {
			policy.Strategy = AccountStrategyCLIPreferred
		}
	case AccountStrategyCLIPreferred:
		policy.ActiveProfileKey = ""
	default:
		policy.Strategy = AccountStrategyCLIPreferred
		policy.ActiveProfileKey = ""
	}
	return policy
}

func CodexProfileKey(userAgent, originator string) (profileKey, family string, ok bool) {
	userAgent = strings.TrimSpace(userAgent)
	originator = strings.TrimSpace(originator)
	uaKey, uaFamily := classifyCodexUserAgent(userAgent)
	originatorKey, originatorFamily := classifyCodexOriginator(originator)

	// A present but unknown identity signal is not safe to merge into a known
	// profile. Reject it instead of persisting a bundle with mismatched parts.
	if (userAgent != "" && uaKey == "") || (originator != "" && originatorKey == "") {
		return "", ProfileFamilyUnknown, false
	}
	if uaKey != "" && originatorKey != "" && uaKey != originatorKey {
		return "", ProfileFamilyUnknown, false
	}
	if originatorKey != "" {
		return originatorKey, originatorFamily, true
	}
	if uaKey != "" {
		return uaKey, uaFamily, true
	}
	return "", ProfileFamilyUnknown, false
}

func CodexProfileFamily(profileKey string) string {
	profileKey = strings.ToLower(strings.TrimSpace(profileKey))
	switch profileKey {
	case ProfileKeyCodexDesktop, "codex_app", "codex_chatgpt_desktop", "codex_atlas":
		return ProfileFamilyDesktop
	case "codex_vscode":
		return ProfileFamilyIDE
	case "codex_cli_rs", "codex-tui", "codex_tui", "codex_exec", "codex_sdk_ts":
		return ProfileFamilyCLI
	default:
		return ProfileFamilyUnknown
	}
}

func CodexProfileOutboundEligibility(record *LearnedRecord) (bool, string) {
	if record == nil || record.Provider != ProviderCodex {
		return false, "invalid_provider"
	}
	profileKey := strings.TrimSpace(record.ProfileKey)
	userAgent := learnedField(record, FieldUserAgent)
	originator := learnedField(record, FieldCodexOriginator)
	if profileKey == "" || userAgent == "" || originator == "" {
		return false, "missing_identity_fields"
	}
	classifiedKey, family, ok := CodexProfileKey(userAgent, originator)
	if !ok {
		return false, "conflicting_identity_fields"
	}
	if classifiedKey != profileKey {
		return false, "profile_key_mismatch"
	}
	switch family {
	case ProfileFamilyCLI, ProfileFamilyIDE, ProfileFamilyDesktop:
		return true, ""
	default:
		return false, "unsupported_profile_family"
	}
}

func SelectCodexProfile(records []LearnedRecord, policy AccountPolicy) ProfileSelection {
	policy = NormalizeAccountPolicy(ProviderCodex, policy.AccountKey, policy)
	profiles := make([]LearnedRecord, 0, len(records))
	for i := range records {
		record := records[i]
		if record.Provider != ProviderCodex || strings.TrimSpace(record.ProfileKey) == "" {
			continue
		}
		if record.ProfileFamily == "" {
			record.ProfileFamily = CodexProfileFamily(record.ProfileKey)
		}
		profiles = append(profiles, record)
	}
	sort.SliceStable(profiles, func(i, j int) bool {
		if profiles[i].LastSeenAt.Equal(profiles[j].LastSeenAt) {
			return profiles[i].ProfileKey < profiles[j].ProfileKey
		}
		return profiles[i].LastSeenAt.After(profiles[j].LastSeenAt)
	})

	if policy.Strategy == AccountStrategyActiveProfile {
		for i := range profiles {
			if profiles[i].ProfileKey == policy.ActiveProfileKey {
				if eligible, _ := CodexProfileOutboundEligibility(&profiles[i]); eligible {
					return ProfileSelection{Profile: cloneRecord(&profiles[i]), Policy: policy, Reason: "active_profile"}
				}
				break
			}
		}
	}

	for _, family := range []string{ProfileFamilyCLI, ProfileFamilyIDE, ProfileFamilyDesktop} {
		for i := range profiles {
			if profiles[i].ProfileFamily != family {
				continue
			}
			if eligible, _ := CodexProfileOutboundEligibility(&profiles[i]); !eligible {
				continue
			}
			reason := "cli_preferred"
			if family == ProfileFamilyDesktop {
				reason = "desktop_fallback"
			}
			if policy.Strategy == AccountStrategyActiveProfile {
				reason = "active_profile_missing_" + reason
			}
			return ProfileSelection{Profile: cloneRecord(&profiles[i]), Policy: policy, Reason: reason}
		}
	}
	if len(profiles) > 0 {
		return ProfileSelection{Policy: policy, Reason: "no_selectable_profile"}
	}
	return ProfileSelection{Policy: policy, Reason: "no_profile"}
}

func classifyCodexUserAgent(userAgent string) (string, string) {
	value := strings.ToLower(strings.TrimSpace(userAgent))
	if value == "" {
		return "", ""
	}
	if strings.HasPrefix(value, "codex desktop/") || strings.Contains(value, "(codex desktop;") {
		return ProfileKeyCodexDesktop, ProfileFamilyDesktop
	}
	product, _ := codexProductVersion(userAgent)
	return classifyCodexToken(product)
}

func classifyCodexOriginator(originator string) (string, string) {
	value := strings.ToLower(strings.TrimSpace(originator))
	if value == "" {
		return "", ""
	}
	if value == "codex desktop" {
		return ProfileKeyCodexDesktop, ProfileFamilyDesktop
	}
	return classifyCodexToken(value)
}

func classifyCodexToken(value string) (string, string) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "codex_cli_rs":
		return "codex_cli_rs", ProfileFamilyCLI
	case "codex-tui", "codex_tui":
		return "codex-tui", ProfileFamilyCLI
	case "codex_exec":
		return "codex_exec", ProfileFamilyCLI
	case "codex_sdk_ts":
		return "codex_sdk_ts", ProfileFamilyCLI
	case "codex_vscode":
		return "codex_vscode", ProfileFamilyIDE
	case "codex_app", "codex_chatgpt_desktop", "codex_atlas":
		return value, ProfileFamilyDesktop
	default:
		return "", ""
	}
}

func CanonicalProfileKey(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	var out strings.Builder
	lastUnderscore := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			out.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			out.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.Trim(out.String(), "_")
}

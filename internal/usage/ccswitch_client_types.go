package usage

import "strings"

// ccSwitchClientTypes lists the CC Switch desktop apps an import preset can
// target, in the order the panel shows them. claude, codex and gemini switch a
// single active provider; opencode, openclaw and hermes are CC Switch's
// additive apps that keep several providers side by side.
//
// GET /ccswitch-import-configs reports this list as "client-types": the panel
// updates itself from the latest release, so it can land on a backend older
// than itself and must only offer the apps that backend accepts.
var ccSwitchClientTypes = []string{"claude", "codex", "gemini", "opencode", "openclaw", "hermes"}

// CcSwitchClientTypes returns the accepted client-type values in display order.
func CcSwitchClientTypes() []string {
	return append([]string(nil), ccSwitchClientTypes...)
}

// IsCcSwitchClientType reports whether value names an accepted client-type.
// Matching ignores case and surrounding spaces, like the stored value.
func IsCcSwitchClientType(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, clientType := range ccSwitchClientTypes {
		if clientType == normalized {
			return true
		}
	}
	return false
}

// CcSwitchClientUsesOpenAIProtocol reports whether the app behind a preset
// reaches its route through the OpenAI routes: Codex calls /v1/responses, and
// OpenCode, OpenClaw and Hermes call /v1/chat/completions because a CC Switch
// link without apiFormat imports them as OpenAI Chat Completions providers.
// Those requests carry the model in the JSON body, so the route rewrites
// request-model to target-model there and /v1/models lists the request names.
// Claude Code has its own rewrite in the Claude handler; Gemini CLI puts the
// model in the URL path and gets no rewrite.
func CcSwitchClientUsesOpenAIProtocol(clientType string) bool {
	switch strings.ToLower(strings.TrimSpace(clientType)) {
	case "codex", "opencode", "openclaw", "hermes":
		return true
	default:
		return false
	}
}

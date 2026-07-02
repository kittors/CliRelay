package codexadmission

import (
	"fmt"
	"net/http"
	"strings"
)

const (
	AllowedClientClaudeCode = "claude_code"

	ReasonDisabled             = "codex_cli_only_disabled"
	ReasonMatchedUserAgent     = "official_client_user_agent_matched"
	ReasonMatchedOriginator    = "official_client_originator_matched"
	ReasonMatchedAllowedClient = "allowed_client_matched"
	ReasonMatchedGlobalAllowed = "global_allowed_client_matched"
	ReasonNotMatched           = "official_client_user_agent_not_matched"
)

var officialUserAgentPrefixes = []string{
	"codex_cli_rs/",
	"codex_vscode/",
	"codex_app/",
	"codex_chatgpt_desktop/",
	"codex_atlas/",
	"codex_exec/",
	"codex_sdk_ts/",
	"codex ",
}

var officialOriginatorPrefixes = []string{
	"codex_",
	"codex ",
}

type AllowedClientEntry struct {
	ID          string
	Label       string
	Description string
	Originator  string
	UAContains  []string
}

type AllowedClientPresetInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

var allowedClientOrder = []string{AllowedClientClaudeCode}

var allowedClientRegistry = map[string]AllowedClientEntry{
	AllowedClientClaudeCode: {
		ID:          AllowedClientClaudeCode,
		Label:       "Claude Code",
		Description: "Allow the Claude Code Codex plugin when Originator and User-Agent both match its fixed signature.",
		Originator:  "Claude Code",
		UAContains:  []string{"Claude Code/"},
	},
}

type Config struct {
	Enabled                    bool
	AllowedClientPresets       []string
	GlobalAllowedClientPresets []string
}

type Result struct {
	Enabled       bool
	Matched       bool
	Reason        string
	MatchedPreset string
}

func AvailableAllowedClientPresets() []AllowedClientPresetInfo {
	out := make([]AllowedClientPresetInfo, 0, len(allowedClientOrder))
	for _, id := range allowedClientOrder {
		entry, ok := allowedClientRegistry[id]
		if !ok {
			continue
		}
		out = append(out, AllowedClientPresetInfo{
			ID:          entry.ID,
			Label:       entry.Label,
			Description: entry.Description,
		})
	}
	return out
}

func NormalizeAllowedClientPresets(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := normalizeClientHeader(value)
		if id == "" {
			continue
		}
		if _, ok := allowedClientRegistry[id]; !ok {
			return nil, fmt.Errorf("unknown codex allowed client preset %q", strings.TrimSpace(value))
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func FilterKnownAllowedClientPresets(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := normalizeClientHeader(value)
		if id == "" {
			continue
		}
		if _, ok := allowedClientRegistry[id]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func IsCodexOfficialClientRequest(userAgent string) bool {
	ua := normalizeClientHeader(userAgent)
	if ua == "" {
		return false
	}
	return matchHeaderPrefixes(ua, officialUserAgentPrefixes)
}

func IsCodexOfficialClientOriginator(originator string) bool {
	value := normalizeClientHeader(originator)
	if value == "" {
		return false
	}
	return matchHeaderPrefixes(value, officialOriginatorPrefixes)
}

func IsCodexOfficialClientByHeaders(userAgent, originator string) bool {
	return IsCodexOfficialClientRequest(userAgent) || IsCodexOfficialClientOriginator(originator)
}

func IsAllowedClientMatch(userAgent, originator string, entry AllowedClientEntry) bool {
	wantOriginator := normalizeClientHeader(entry.Originator)
	if wantOriginator == "" {
		return false
	}
	if normalizeClientHeader(originator) != wantOriginator {
		return false
	}
	if len(entry.UAContains) == 0 {
		return false
	}
	ua := normalizeClientHeader(userAgent)
	for _, marker := range entry.UAContains {
		normalizedMarker := normalizeClientHeader(marker)
		if normalizedMarker == "" {
			return false
		}
		if !strings.Contains(ua, normalizedMarker) {
			return false
		}
	}
	return true
}

func MatchAllowedClients(userAgent, originator string, presetIDs []string) (bool, string) {
	for _, id := range presetIDs {
		normalizedID := normalizeClientHeader(id)
		entry, ok := allowedClientRegistry[normalizedID]
		if !ok {
			continue
		}
		if IsAllowedClientMatch(userAgent, originator, entry) {
			return true, normalizedID
		}
	}
	return false, ""
}

func Evaluate(userAgent, originator string, cfg Config) Result {
	if !cfg.Enabled {
		return Result{Enabled: false, Matched: false, Reason: ReasonDisabled}
	}
	if IsCodexOfficialClientRequest(userAgent) {
		return Result{Enabled: true, Matched: true, Reason: ReasonMatchedUserAgent}
	}
	if IsCodexOfficialClientOriginator(originator) {
		return Result{Enabled: true, Matched: true, Reason: ReasonMatchedOriginator}
	}
	if matched, preset := MatchAllowedClients(userAgent, originator, cfg.AllowedClientPresets); matched {
		return Result{Enabled: true, Matched: true, Reason: ReasonMatchedAllowedClient, MatchedPreset: preset}
	}
	if matched, preset := MatchAllowedClients(userAgent, originator, cfg.GlobalAllowedClientPresets); matched {
		return Result{Enabled: true, Matched: true, Reason: ReasonMatchedGlobalAllowed, MatchedPreset: preset}
	}
	return Result{Enabled: true, Matched: false, Reason: ReasonNotMatched}
}

func EvaluateHeaders(headers http.Header, cfg Config) Result {
	if headers == nil {
		return Evaluate("", "", cfg)
	}
	return Evaluate(headers.Get("User-Agent"), headers.Get("Originator"), cfg)
}

func normalizeClientHeader(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func matchHeaderPrefixes(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		normalizedPrefix := normalizeClientHeader(prefix)
		if normalizedPrefix == "" {
			continue
		}
		if strings.HasPrefix(value, normalizedPrefix) || strings.Contains(value, normalizedPrefix) {
			return true
		}
	}
	return false
}

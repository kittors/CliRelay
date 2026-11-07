package contentmoderation

import "strings"

// maskClients name the clients the records tell apart by their User-Agent, in
// the order they are tried: Claude Code's says claude-cli, and Codex's
// codex_cli_rs, codex_exec or codex-tui.
var maskClients = []struct {
	id    string
	marks []string
}{
	{"claude-code", []string{"claude-cli", "claude-code", "claude_code"}},
	{"gemini-cli", []string{"geminicli", "gemini-cli"}},
	{"codex", []string{"codex"}},
	{"opencode", []string{"opencode"}},
	{"kilo-code", []string{"kilo-code", "kilocode"}},
	{"roo-code", []string{"roo-code", "roocode"}},
	{"cline", []string{"cline"}},
	{"cursor", []string{"cursor"}},
}

// MaskClient is the client a User-Agent names, for the records' flow chart
// and filters, "other" when it is none of maskClients.
func MaskClient(userAgent string) string {
	ua := strings.ToLower(userAgent)
	for _, c := range maskClients {
		for _, mark := range c.marks {
			if strings.Contains(ua, mark) {
				return c.id
			}
		}
	}
	return "other"
}

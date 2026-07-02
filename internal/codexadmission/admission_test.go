package codexadmission

import "testing"

const (
	testClaudeCodeOriginator = "Claude Code"
	testClaudeCodeUserAgent  = "Claude Code/0.5.0 (Macos 15.5; arm64) iTerm2.app (Claude Code; 1.0.4)"
)

func TestIsCodexOfficialClientRequest(t *testing.T) {
	tests := []struct {
		name string
		ua   string
		want bool
	}{
		{name: "codex cli rs", ua: "codex_cli_rs/0.130.0", want: true},
		{name: "codex vscode", ua: "codex_vscode/1.2.3", want: true},
		{name: "codex app", ua: "codex_app/0.1.0", want: true},
		{name: "codex chatgpt desktop", ua: "codex_chatgpt_desktop/1.0.0", want: true},
		{name: "codex atlas", ua: "codex_atlas/1.0.0", want: true},
		{name: "codex exec", ua: "codex_exec/0.1.0", want: true},
		{name: "codex sdk ts", ua: "codex_sdk_ts/0.1.0", want: true},
		{name: "codex desktop token", ua: "Codex Desktop/1.2.3", want: true},
		{name: "composite browser ua", ua: "Mozilla/5.0 codex_cli_rs/0.130.0", want: true},
		{name: "not codex", ua: "curl/8.0.1", want: false},
		{name: "empty", ua: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCodexOfficialClientRequest(tt.ua); got != tt.want {
				t.Fatalf("IsCodexOfficialClientRequest(%q) = %v, want %v", tt.ua, got, tt.want)
			}
		})
	}
}

func TestIsCodexOfficialClientOriginator(t *testing.T) {
	tests := []struct {
		name       string
		originator string
		want       bool
	}{
		{name: "codex cli rs", originator: "codex_cli_rs", want: true},
		{name: "codex vscode", originator: "codex_vscode", want: true},
		{name: "codex app", originator: "codex_app", want: true},
		{name: "codex desktop", originator: "Codex Desktop", want: true},
		{name: "not codex", originator: "Claude Code", want: false},
		{name: "empty", originator: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCodexOfficialClientOriginator(tt.originator); got != tt.want {
				t.Fatalf("IsCodexOfficialClientOriginator(%q) = %v, want %v", tt.originator, got, tt.want)
			}
		})
	}
}

func TestIsAllowedClientMatchRequiresClaudeCodeTwoFactorSignature(t *testing.T) {
	entry := AllowedClientEntry{Originator: "Claude Code", UAContains: []string{"Claude Code/"}}
	tests := []struct {
		name       string
		ua         string
		originator string
		want       bool
	}{
		{name: "real signature", ua: testClaudeCodeUserAgent, originator: testClaudeCodeOriginator, want: true},
		{name: "case insensitive", ua: "claude code/0.5.0 (macos)", originator: "claude code", want: true},
		{name: "originator trimmed", ua: testClaudeCodeUserAgent, originator: "  Claude Code  ", want: true},
		{name: "originator suffix rejected", ua: testClaudeCodeUserAgent, originator: "Claude Code Extra", want: false},
		{name: "missing originator rejected", ua: testClaudeCodeUserAgent, originator: "", want: false},
		{name: "codex originator rejected", ua: testClaudeCodeUserAgent, originator: "codex_cli_rs", want: false},
		{name: "missing ua marker rejected", ua: "curl/8.0", originator: testClaudeCodeOriginator, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAllowedClientMatch(tt.ua, tt.originator, entry); got != tt.want {
				t.Fatalf("IsAllowedClientMatch(%q, %q) = %v, want %v", tt.ua, tt.originator, got, tt.want)
			}
		})
	}
}

func TestAllowedClientInvalidEntryNeverMatches(t *testing.T) {
	tests := []struct {
		name  string
		entry AllowedClientEntry
	}{
		{name: "empty originator", entry: AllowedClientEntry{UAContains: []string{"Claude Code/"}}},
		{name: "empty ua markers", entry: AllowedClientEntry{Originator: "Claude Code"}},
		{name: "blank ua marker", entry: AllowedClientEntry{Originator: "Claude Code", UAContains: []string{"   "}}},
		{name: "mixed blank ua marker", entry: AllowedClientEntry{Originator: "Claude Code", UAContains: []string{"", "Claude Code/"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsAllowedClientMatch(testClaudeCodeUserAgent, testClaudeCodeOriginator, tt.entry) {
				t.Fatal("invalid allowed client entry matched")
			}
		})
	}
}

func TestNormalizeAllowedClientPresetsRejectsUnknown(t *testing.T) {
	got, err := NormalizeAllowedClientPresets([]string{"  Claude_Code ", "claude_code"})
	if err != nil {
		t.Fatalf("NormalizeAllowedClientPresets() error = %v", err)
	}
	if len(got) != 1 || got[0] != AllowedClientClaudeCode {
		t.Fatalf("NormalizeAllowedClientPresets() = %#v, want [claude_code]", got)
	}

	if _, err := NormalizeAllowedClientPresets([]string{"unknown_client"}); err == nil {
		t.Fatal("NormalizeAllowedClientPresets() error = nil, want unknown preset error")
	}
}

func TestEvaluateCodexAdmission(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		ua         string
		originator string
		want       Result
	}{
		{name: "disabled", cfg: Config{}, ua: "curl/8", want: Result{Enabled: false, Matched: false, Reason: ReasonDisabled}},
		{name: "official ua", cfg: Config{Enabled: true}, ua: "codex_cli_rs/0.130.0", want: Result{Enabled: true, Matched: true, Reason: ReasonMatchedUserAgent}},
		{name: "official originator", cfg: Config{Enabled: true}, ua: "curl/8", originator: "codex_vscode", want: Result{Enabled: true, Matched: true, Reason: ReasonMatchedOriginator}},
		{name: "claude code preset", cfg: Config{Enabled: true, AllowedClientPresets: []string{AllowedClientClaudeCode}}, ua: testClaudeCodeUserAgent, originator: testClaudeCodeOriginator, want: Result{Enabled: true, Matched: true, Reason: ReasonMatchedAllowedClient, MatchedPreset: AllowedClientClaudeCode}},
		{name: "global claude code preset", cfg: Config{Enabled: true, GlobalAllowedClientPresets: []string{AllowedClientClaudeCode}}, ua: testClaudeCodeUserAgent, originator: testClaudeCodeOriginator, want: Result{Enabled: true, Matched: true, Reason: ReasonMatchedGlobalAllowed, MatchedPreset: AllowedClientClaudeCode}},
		{name: "account preset wins before global", cfg: Config{Enabled: true, AllowedClientPresets: []string{AllowedClientClaudeCode}, GlobalAllowedClientPresets: []string{AllowedClientClaudeCode}}, ua: testClaudeCodeUserAgent, originator: testClaudeCodeOriginator, want: Result{Enabled: true, Matched: true, Reason: ReasonMatchedAllowedClient, MatchedPreset: AllowedClientClaudeCode}},
		{name: "claude code missing ua marker rejected", cfg: Config{Enabled: true, AllowedClientPresets: []string{AllowedClientClaudeCode}}, ua: "curl/8", originator: testClaudeCodeOriginator, want: Result{Enabled: true, Matched: false, Reason: ReasonNotMatched}},
		{name: "unknown preset ignored", cfg: Config{Enabled: true, AllowedClientPresets: []string{"unknown_client"}}, ua: testClaudeCodeUserAgent, originator: testClaudeCodeOriginator, want: Result{Enabled: true, Matched: false, Reason: ReasonNotMatched}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Evaluate(tt.ua, tt.originator, tt.cfg); got != tt.want {
				t.Fatalf("Evaluate() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

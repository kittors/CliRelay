package identityfingerprint

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestCodexVersionBelow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		version string
		want    bool
	}{
		{"0.130.0", true},
		{"0.118.0", true},
		{"0.146.9", true},
		{"0.147.0", false},
		{"0.147.1", false},
		{"0.180.0", false},
		{"0.147.0-alpha.6.6", false}, // pre-release of the floor parses as 0.147.0
		{"", true},
		{"abc", true},
	}
	for _, tc := range cases {
		if got := codexVersionBelow(tc.version, minCodexClientVersion); got != tc.want {
			t.Fatalf("codexVersionBelow(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestRewriteCodexUserAgentVersion(t *testing.T) {
	t.Parallel()
	in := "codex_exec/0.130.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.130.0)"
	want := "codex_exec/0.147.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.147.0)"
	if got := rewriteCodexUserAgentVersion(in, "0.147.0"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Single-component numbers (e.g. the "3" in a terminal name) must survive.
	in2 := "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	got2 := rewriteCodexUserAgentVersion(in2, "0.147.0")
	want2 := "codex-tui/0.147.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.147.0)"
	if got2 != want2 {
		t.Fatalf("got %q, want %q", got2, want2)
	}
}

func TestResolveCodexFloorsLearnedStaleVersion(t *testing.T) {
	t.Parallel()
	learned := &LearnedRecord{
		Provider:   ProviderCodex,
		ProfileKey: "codex_exec",
		Version:    "0.130.0",
		Fields: map[string]string{
			FieldUserAgent:       "codex_exec/0.130.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.130.0)",
			FieldCodexVersion:    "0.130.0",
			FieldCodexOriginator: "codex_exec",
		},
	}
	resolved, _ := ResolveCodex(config.CodexIdentityFingerprintConfig{Enabled: true}, learned)
	if resolved.Version != minCodexClientVersion {
		t.Fatalf("Version = %q, want floored to %q", resolved.Version, minCodexClientVersion)
	}
	wantUA := "codex_exec/0.147.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.147.0)"
	if resolved.UserAgent != wantUA {
		t.Fatalf("UserAgent = %q, want %q", resolved.UserAgent, wantUA)
	}
	if resolved.Originator != "codex_exec" {
		t.Fatalf("Originator = %q, want learned codex_exec preserved", resolved.Originator)
	}
}

func TestResolveCodexKeepsNewerLearnedVersion(t *testing.T) {
	t.Parallel()
	learned := &LearnedRecord{
		Provider:   ProviderCodex,
		ProfileKey: ProfileKeyCodexDesktop,
		Version:    "0.180.0",
		Fields: map[string]string{
			FieldUserAgent:       "Codex Desktop/0.180.0 (Windows 10.0.26220; x86_64) unknown (Codex Desktop; 26.803.81509)",
			FieldCodexVersion:    "0.180.0",
			FieldCodexOriginator: "Codex Desktop",
		},
	}
	resolved, _ := ResolveCodex(config.CodexIdentityFingerprintConfig{Enabled: true}, learned)
	if resolved.Version != "0.180.0" {
		t.Fatalf("Version = %q, want learned 0.180.0 preserved", resolved.Version)
	}
	if resolved.UserAgent != learned.Fields[FieldUserAgent] {
		t.Fatalf("UserAgent = %q, want learned UA untouched", resolved.UserAgent)
	}
}

func TestResolveCodexProfileFloorsStaleVersion(t *testing.T) {
	t.Parallel()
	profile := &LearnedRecord{
		Provider:   ProviderCodex,
		ProfileKey: "codex_exec",
		Version:    "0.130.0",
		Fields: map[string]string{
			FieldUserAgent:       "codex_exec/0.130.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.130.0)",
			FieldCodexVersion:    "0.130.0",
			FieldCodexOriginator: "codex_exec",
		},
	}
	resolved, _ := ResolveCodexProfile(config.CodexIdentityFingerprintConfig{Enabled: true}, profile)
	if resolved.Version != minCodexClientVersion {
		t.Fatalf("Version = %q, want floored to %q", resolved.Version, minCodexClientVersion)
	}
	wantUA := "codex_exec/0.147.0 (Debian 13.0.0; aarch64) xterm-256color (codex_exec; 0.147.0)"
	if resolved.UserAgent != wantUA {
		t.Fatalf("UserAgent = %q, want %q", resolved.UserAgent, wantUA)
	}
}

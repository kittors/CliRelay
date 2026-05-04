package config

import "testing"

func TestDefaultCodexIdentityFingerprintUsesCurrentVersionAndDynamicSessions(t *testing.T) {
	t.Parallel()

	got := DefaultCodexIdentityFingerprint()

	if got.Enabled {
		t.Fatalf("Enabled = true, want false by default")
	}
	if got.Version != "" {
		t.Fatalf("Version = %q, want empty (codex-tui does not require Version)", got.Version)
	}
	if got.UserAgent != "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)" {
		t.Fatalf("UserAgent = %q, want codex-tui user agent", got.UserAgent)
	}
	if got.SessionMode != "per-request" {
		t.Fatalf("SessionMode = %q, want per-request", got.SessionMode)
	}
}

func TestNormalizeCodexIdentityFingerprintAppliesCurrentDefaults(t *testing.T) {
	t.Parallel()

	got := NormalizeCodexIdentityFingerprint(CodexIdentityFingerprintConfig{})

	if got.Version != "" {
		t.Fatalf("Version = %q, want empty (codex-tui does not require Version)", got.Version)
	}
	if got.UserAgent != "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)" {
		t.Fatalf("UserAgent = %q, want codex-tui user agent", got.UserAgent)
	}
	if got.SessionMode != "per-request" {
		t.Fatalf("SessionMode = %q, want per-request", got.SessionMode)
	}
}

func TestDefaultClaudeIdentityFingerprintMirrorsClaudeCode(t *testing.T) {
	t.Parallel()

	got := DefaultClaudeIdentityFingerprint()

	if got.Enabled {
		t.Fatalf("Enabled = true, want false by default")
	}
	if got.CLIVersion != "2.1.88" {
		t.Fatalf("CLIVersion = %q, want 2.1.88", got.CLIVersion)
	}
	if got.Entrypoint != "cli" {
		t.Fatalf("Entrypoint = %q, want cli", got.Entrypoint)
	}
	if got.UserAgent != "claude-cli/2.1.88 (external, cli)" {
		t.Fatalf("UserAgent = %q, want Claude Code user agent", got.UserAgent)
	}
	if got.StainlessPackageVersion != "0.74.0" {
		t.Fatalf("StainlessPackageVersion = %q, want 0.74.0", got.StainlessPackageVersion)
	}
	if got.StainlessRuntimeVersion != "v22.13.0" {
		t.Fatalf("StainlessRuntimeVersion = %q, want v22.13.0", got.StainlessRuntimeVersion)
	}
	if got.SessionMode != "per-request" {
		t.Fatalf("SessionMode = %q, want per-request", got.SessionMode)
	}
}

func TestNormalizeClaudeIdentityFingerprintBuildsUserAgentFromVersionAndEntrypoint(t *testing.T) {
	t.Parallel()

	got := NormalizeClaudeIdentityFingerprint(ClaudeIdentityFingerprintConfig{
		Enabled:     true,
		CLIVersion:  " 2.2.0 ",
		Entrypoint:  " sdk-cli ",
		SessionMode: "INVALID",
		CustomHeaders: map[string]string{
			" X-Test ": " value ",
			"":         "discard",
			"X-Blank":  " ",
		},
	})

	if !got.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if got.CLIVersion != "2.2.0" {
		t.Fatalf("CLIVersion = %q, want 2.2.0", got.CLIVersion)
	}
	if got.Entrypoint != "sdk-cli" {
		t.Fatalf("Entrypoint = %q, want sdk-cli", got.Entrypoint)
	}
	if got.UserAgent != "claude-cli/2.2.0 (external, sdk-cli)" {
		t.Fatalf("UserAgent = %q, want derived Claude Code user agent", got.UserAgent)
	}
	if got.SessionMode != "per-request" {
		t.Fatalf("SessionMode = %q, want per-request fallback", got.SessionMode)
	}
	if got.CustomHeaders["X-Test"] != "value" || len(got.CustomHeaders) != 1 {
		t.Fatalf("CustomHeaders = %#v, want trimmed non-empty header only", got.CustomHeaders)
	}
}

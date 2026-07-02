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

func TestCleanCodexIdentityFingerprintPreservesEmptyAutomaticFields(t *testing.T) {
	t.Parallel()

	got := CleanCodexIdentityFingerprint(CodexIdentityFingerprintConfig{
		Enabled:     true,
		UserAgent:   " ",
		SessionMode: "INVALID",
		CustomHeaders: map[string]string{
			" X-Test ": " value ",
			"X-Blank":  " ",
		},
	})

	if !got.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if got.UserAgent != "" {
		t.Fatalf("UserAgent = %q, want empty automatic field", got.UserAgent)
	}
	if got.SessionMode != "per-request" {
		t.Fatalf("SessionMode = %q, want safe fallback for invalid non-empty value", got.SessionMode)
	}
	if got.CustomHeaders["X-Test"] != "value" || len(got.CustomHeaders) != 1 {
		t.Fatalf("CustomHeaders = %#v, want trimmed non-empty header only", got.CustomHeaders)
	}
}

func TestDefaultClaudeIdentityFingerprintMirrorsClaudeCode(t *testing.T) {
	t.Parallel()

	got := DefaultClaudeIdentityFingerprint()

	if got.Enabled {
		t.Fatalf("Enabled = true, want false by default")
	}
	if got.CLIVersion != "2.1.161" {
		t.Fatalf("CLIVersion = %q, want 2.1.161", got.CLIVersion)
	}
	if got.Entrypoint != "cli" {
		t.Fatalf("Entrypoint = %q, want cli", got.Entrypoint)
	}
	if got.UserAgent != "claude-cli/2.1.161 (external, cli)" {
		t.Fatalf("UserAgent = %q, want Claude Code user agent", got.UserAgent)
	}
	if got.StainlessPackageVersion != "0.94.0" {
		t.Fatalf("StainlessPackageVersion = %q, want 0.94.0", got.StainlessPackageVersion)
	}
	if got.StainlessRuntimeVersion != "v24.3.0" {
		t.Fatalf("StainlessRuntimeVersion = %q, want v24.3.0", got.StainlessRuntimeVersion)
	}
	if got.AnthropicBeta != "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24,context-management-2025-06-27,extended-cache-ttl-2025-04-11" {
		t.Fatalf("AnthropicBeta = %q, want Claude Code OAuth beta set", got.AnthropicBeta)
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

func TestCleanClaudeIdentityFingerprintPreservesEmptyAutomaticFields(t *testing.T) {
	t.Parallel()

	got := CleanClaudeIdentityFingerprint(ClaudeIdentityFingerprintConfig{
		Enabled:    true,
		CLIVersion: " ",
		Entrypoint: " cli ",
	})

	if !got.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if got.CLIVersion != "" {
		t.Fatalf("CLIVersion = %q, want empty automatic field", got.CLIVersion)
	}
	if got.Entrypoint != "cli" {
		t.Fatalf("Entrypoint = %q, want trimmed custom field", got.Entrypoint)
	}
	if got.UserAgent != "" {
		t.Fatalf("UserAgent = %q, want empty automatic field", got.UserAgent)
	}
}

func TestDefaultGeminiIdentityFingerprint(t *testing.T) {
	t.Parallel()

	got := DefaultGeminiIdentityFingerprint()

	if got.Enabled {
		t.Fatal("Enabled = true, want false by default")
	}
	if got.UserAgent != "google-api-nodejs-client/9.15.1" {
		t.Fatalf("UserAgent = %q, want Gemini CLI default", got.UserAgent)
	}
	if got.APIClient != "gl-node/22.17.0" {
		t.Fatalf("APIClient = %q, want gl-node default", got.APIClient)
	}
	if got.ClientMetadata != "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI" {
		t.Fatalf("ClientMetadata = %q, want Gemini CLI metadata", got.ClientMetadata)
	}
}

package identityfingerprint

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestExtractClaudeObservationFromRealClientHeaders(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.170 (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "oauth-2025-04-20")
	headers.Set("X-Stainless-Package-Version", "0.95.0")
	headers.Set("X-Stainless-Runtime-Version", "v24.4.0")
	headers.Set("X-Stainless-Timeout", "700")

	obs, ok := ExtractObservation(LearnInput{
		Provider:   ProviderClaude,
		AccountKey: "acct",
		Headers:    headers,
		ObservedAt: time.Date(2026, 6, 23, 1, 2, 3, 0, time.UTC),
	})
	if !ok {
		t.Fatal("ExtractObservation returned false")
	}
	if obs.ClientProduct != "claude-cli" || obs.Version != "2.1.170" {
		t.Fatalf("product/version = %s/%s", obs.ClientProduct, obs.Version)
	}
	if obs.Fields[FieldClaudeStainlessRuntime] != "v24.4.0" {
		t.Fatalf("runtime = %q, want learned header", obs.Fields[FieldClaudeStainlessRuntime])
	}
}

func TestExtractCodexObservationFromDesktopUserAgent(t *testing.T) {
	tests := []struct {
		name string
		ua   string
	}{
		{
			name: "windows desktop",
			ua:   "Codex Desktop/0.142.0 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.616.81150)",
		},
		{
			name: "mac desktop",
			ua:   "Codex Desktop/0.142.0 (Mac OS 26.5.1; arm64) unknown (Codex Desktop; 26.616.81150)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("User-Agent", tt.ua)
			headers.Set("Originator", "Codex Desktop")
			headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")

			obs, ok := ExtractObservation(LearnInput{
				Provider:   ProviderCodex,
				AccountKey: "acct",
				Headers:    headers,
				ObservedAt: time.Date(2026, 6, 24, 1, 2, 3, 0, time.UTC),
			})
			if !ok {
				t.Fatal("ExtractObservation returned false")
			}
			if obs.ClientProduct != "codex" || obs.ClientVariant != "Codex Desktop" {
				t.Fatalf("product/variant = %s/%s, want codex/Codex Desktop", obs.ClientProduct, obs.ClientVariant)
			}
			if obs.Version != "0.142.0" || obs.Fields[FieldCodexVersion] != "0.142.0" {
				t.Fatalf("version fields = %s/%s, want 0.142.0", obs.Version, obs.Fields[FieldCodexVersion])
			}
		})
	}
}

func TestExtractCodexObservationVersionHeaderOverridesUserAgentVersion(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "Codex Desktop/0.142.0 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.616.81150)")
	headers.Set("Version", "0.143.1")
	headers.Set("Originator", "Codex Desktop")

	obs, ok := ExtractObservation(LearnInput{
		Provider:   ProviderCodex,
		AccountKey: "acct",
		Headers:    headers,
		ObservedAt: time.Date(2026, 6, 24, 1, 2, 3, 0, time.UTC),
	})
	if !ok {
		t.Fatal("ExtractObservation returned false")
	}
	if obs.Version != "0.143.1" || obs.Fields[FieldCodexVersion] != "0.143.1" {
		t.Fatalf("version fields = %s/%s, want Version header", obs.Version, obs.Fields[FieldCodexVersion])
	}
}

func TestMergeObservationUpdatesOnlyNewerSameProductAndPreservesMissingFields(t *testing.T) {
	existing := &LearnedRecord{
		Provider:      ProviderClaude,
		AccountKey:    "acct",
		ClientProduct: "claude-cli",
		Version:       "2.1.160",
		Fields: map[string]string{
			FieldUserAgent:              "claude-cli/2.1.160 (external, cli)",
			FieldClaudeStainlessRuntime: "v24.3.0",
			FieldClaudeStainlessTimeout: "600",
		},
		CreatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		LastSeenAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	obs := Observation{
		Provider:      ProviderClaude,
		AccountKey:    "acct",
		ClientProduct: "claude-cli",
		Version:       "2.1.170",
		Fields: map[string]string{
			FieldUserAgent:        "claude-cli/2.1.170 (external, cli)",
			FieldClaudeCLIVersion: "2.1.170",
		},
		ObservedAt: time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC),
	}

	result := MergeObservation(existing, obs)
	if !result.Changed || result.Reason != "merged_newer_version" {
		t.Fatalf("merge result = %+v, want newer merge", result)
	}
	if got := result.Record.Fields[FieldClaudeStainlessRuntime]; got != "v24.3.0" {
		t.Fatalf("runtime = %q, want preserved missing field", got)
	}
	if got := result.Record.Fields[FieldUserAgent]; got != "claude-cli/2.1.170 (external, cli)" {
		t.Fatalf("User-Agent = %q, want newer learned UA", got)
	}
}

func TestMergeObservationIgnoresDifferentProduct(t *testing.T) {
	existing := &LearnedRecord{
		Provider:      ProviderCodex,
		AccountKey:    "acct",
		ClientProduct: "codex-tui",
		Version:       "0.118.0",
		Fields: map[string]string{
			FieldUserAgent: "codex-tui/0.118.0 (Mac OS 26.3.1; arm64)",
		},
	}
	obs := Observation{
		Provider:      ProviderCodex,
		AccountKey:    "acct",
		ClientProduct: "curl",
		Version:       "9.0.0",
		Fields: map[string]string{
			FieldUserAgent: "curl/9.0.0",
		},
		ObservedAt: time.Now().UTC(),
	}

	result := MergeObservation(existing, obs)
	if result.Reason != "different_product_last_seen" {
		t.Fatalf("reason = %q, want different_product_last_seen", result.Reason)
	}
	if got := result.Record.Fields[FieldUserAgent]; got != "codex-tui/0.118.0 (Mac OS 26.3.1; arm64)" {
		t.Fatalf("User-Agent = %q, want existing record preserved", got)
	}
}

func TestResolveClaudeUsesFieldLevelLearnedPresetBuiltinPriority(t *testing.T) {
	learned := &LearnedRecord{
		Provider:      ProviderClaude,
		AccountKey:    "acct",
		ClientProduct: "claude-cli",
		Version:       "2.1.170",
		Fields: map[string]string{
			FieldUserAgent:              "claude-cli/2.1.170 (external, cli)",
			FieldClaudeCLIVersion:       "2.1.170",
			FieldClaudeEntrypoint:       "cli",
			FieldClaudeStainlessRuntime: "v24.4.0",
		},
	}

	fp, effective := ResolveClaude(config.ClaudeIdentityFingerprintConfig{
		Enabled:                 true,
		StainlessRuntimeVersion: "preset-runtime",
	}, learned)

	if fp.UserAgent != "claude-cli/2.1.170 (external, cli)" {
		t.Fatalf("UserAgent = %q, want learned", fp.UserAgent)
	}
	if fp.StainlessRuntimeVersion != "v24.4.0" {
		t.Fatalf("RuntimeVersion = %q, want learned", fp.StainlessRuntimeVersion)
	}
	if got := effective.Fields[FieldUserAgent].Source; got != FieldSourceLearned {
		t.Fatalf("UserAgent source = %q, want learned", got)
	}
	if got := effective.Fields[FieldClaudeStainlessRuntime].Source; got != FieldSourceLearned {
		t.Fatalf("runtime source = %q, want learned", got)
	}
	if got := effective.Fields[FieldClaudeStainlessPackage].Source; got != FieldSourceBuiltinDefault {
		t.Fatalf("package source = %q, want builtin_default", got)
	}
}

func TestResolveCodexUsesPresetBeforeBuiltinDefault(t *testing.T) {
	fp, effective := ResolveCodex(config.CodexIdentityFingerprintConfig{
		Enabled:    true,
		UserAgent:  "codex-tui/0.125.0 (Mac OS 26.5; arm64)",
		Originator: "codex-tui",
	}, nil)

	if fp.UserAgent != "codex-tui/0.125.0 (Mac OS 26.5; arm64)" {
		t.Fatalf("UserAgent = %q, want preset", fp.UserAgent)
	}
	if got := effective.Fields[FieldUserAgent].Source; got != FieldSourcePreset {
		t.Fatalf("UserAgent source = %q, want preset", got)
	}
	if got := effective.Fields[FieldCodexWebsocketBeta].Source; got != FieldSourceBuiltinDefault {
		t.Fatalf("websocket beta source = %q, want builtin_default", got)
	}
}

func TestResolveCodexEffectiveVersionUsesFieldLevelPriority(t *testing.T) {
	learned := &LearnedRecord{
		Provider:   ProviderCodex,
		AccountKey: "acct",
		Fields: map[string]string{
			FieldCodexVersion: "0.142.0",
		},
	}

	fp, effective := ResolveCodex(config.CodexIdentityFingerprintConfig{
		Enabled: true,
		Version: "0.140.0",
	}, learned)

	if fp.Version != "0.142.0" {
		t.Fatalf("resolved version = %q, want learned field", fp.Version)
	}
	if effective.Version != "0.142.0" {
		t.Fatalf("effective version = %q, want learned field", effective.Version)
	}
	if got := effective.Fields[FieldCodexVersion].Source; got != FieldSourceLearned {
		t.Fatalf("version source = %q, want learned", got)
	}
}

func TestResolveClaudeEffectiveVersionFallsBackToPreset(t *testing.T) {
	_, effective := ResolveClaude(config.ClaudeIdentityFingerprintConfig{
		Enabled:    true,
		CLIVersion: "2.1.170",
	}, &LearnedRecord{
		Provider:   ProviderClaude,
		AccountKey: "acct",
		Fields:     map[string]string{},
	})

	if effective.Version != "2.1.170" {
		t.Fatalf("effective version = %q, want preset cli-version", effective.Version)
	}
	if got := effective.Fields[FieldClaudeCLIVersion].Source; got != FieldSourcePreset {
		t.Fatalf("cli-version source = %q, want preset", got)
	}
}

func TestResolveGeminiUsesLearnedWhenPresetEmpty(t *testing.T) {
	learned := &LearnedRecord{
		Provider:   ProviderGemini,
		AccountKey: "acct",
		Fields: map[string]string{
			FieldUserAgent:            "google-api-nodejs-client/9.16.0",
			FieldGeminiAPIClient:      "gl-node/24.0.0",
			FieldGeminiClientMetadata: "pluginType=GEMINI,ideType=IDE_UNSPECIFIED",
		},
	}

	fp, effective := ResolveGemini(config.GeminiIdentityFingerprintConfig{Enabled: true}, learned)
	if fp.UserAgent != "google-api-nodejs-client/9.16.0" || fp.APIClient != "gl-node/24.0.0" {
		t.Fatalf("resolved Gemini fingerprint = %#v, want learned fields", fp)
	}
	if got := effective.Fields[FieldGeminiClientMetadata].Source; got != FieldSourceLearned {
		t.Fatalf("metadata source = %q, want learned", got)
	}
}

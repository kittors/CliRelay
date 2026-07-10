package usage

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
)

func TestObserveIdentityFingerprintLearnsAndMergesClaudeAccount(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	accountKey := "claude-account"
	firstSeen := time.Date(2026, 6, 23, 1, 2, 3, 0, time.UTC)
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.170 (external, cli)")
	headers.Set("X-App", "cli")
	headers.Set("Anthropic-Beta", "claude-code-20250219,oauth-2025-04-20")
	headers.Set("X-Stainless-Package-Version", "0.95.0")
	headers.Set("X-Stainless-Runtime-Version", "v24.4.0")
	headers.Set("X-Stainless-Timeout", "700")

	record, result, err := ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderClaude,
		AccountKey:    accountKey,
		AuthSubjectID: "subject-claude",
		Headers:       headers,
		ObservedAt:    firstSeen,
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint returned error: %v", err)
	}
	if result.Reason != "created" || record == nil {
		t.Fatalf("merge result = %+v, record = %#v, want created", result, record)
	}
	if record.Fields[identityfingerprint.FieldUserAgent] != "claude-cli/2.1.170 (external, cli)" {
		t.Fatalf("learned User-Agent = %q", record.Fields[identityfingerprint.FieldUserAgent])
	}
	if record.Fields[identityfingerprint.FieldClaudeStainlessRuntime] != "v24.4.0" {
		t.Fatalf("learned runtime = %q", record.Fields[identityfingerprint.FieldClaudeStainlessRuntime])
	}
	if record.ObservedHeaders["X-Stainless-Timeout"] != "700" {
		t.Fatalf("observed headers = %#v, want stainless timeout", record.ObservedHeaders)
	}

	olderHeaders := http.Header{}
	olderHeaders.Set("User-Agent", "claude-cli/2.1.100 (external, cli)")
	olderHeaders.Set("X-App", "cli")
	olderHeaders.Set("X-Stainless-Runtime-Version", "v24.0.0")
	olderSeen := firstSeen.Add(time.Hour)
	record, result, err = ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderClaude,
		AccountKey:    accountKey,
		AuthSubjectID: "subject-claude",
		Headers:       olderHeaders,
		ObservedAt:    olderSeen,
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint older returned error: %v", err)
	}
	if result.Reason != "not_newer_last_seen" {
		t.Fatalf("older merge reason = %q, want not_newer_last_seen", result.Reason)
	}
	if record.Fields[identityfingerprint.FieldUserAgent] != "claude-cli/2.1.170 (external, cli)" {
		t.Fatalf("older observation should not replace User-Agent, got %q", record.Fields[identityfingerprint.FieldUserAgent])
	}
	if !record.LastSeenAt.Equal(olderSeen) {
		t.Fatalf("LastSeenAt = %s, want %s", record.LastSeenAt, olderSeen)
	}

	newerHeaders := http.Header{}
	newerHeaders.Set("User-Agent", "claude-cli/2.1.180 (external, cli)")
	newerHeaders.Set("X-App", "cli")
	newerSeen := olderSeen.Add(time.Hour)
	record, result, err = ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderClaude,
		AccountKey:    accountKey,
		AuthSubjectID: "subject-claude",
		Headers:       newerHeaders,
		ObservedAt:    newerSeen,
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint newer returned error: %v", err)
	}
	if result.Reason != "merged_newer_version" {
		t.Fatalf("newer merge reason = %q, want merged_newer_version", result.Reason)
	}
	if record.Version != "2.1.180" {
		t.Fatalf("Version = %q, want newer version", record.Version)
	}
	if record.Fields[identityfingerprint.FieldClaudeStainlessRuntime] != "v24.4.0" {
		t.Fatalf("newer partial observation should preserve runtime, got %q", record.Fields[identityfingerprint.FieldClaudeStainlessRuntime])
	}

	stored, err := GetIdentityFingerprint(identityfingerprint.ProviderClaude, accountKey)
	if err != nil {
		t.Fatalf("GetIdentityFingerprint returned error: %v", err)
	}
	if stored == nil || stored.Version != "2.1.180" {
		t.Fatalf("stored record = %#v, want newer version", stored)
	}
	list, err := ListIdentityFingerprints(identityfingerprint.ProviderClaude, 10)
	if err != nil {
		t.Fatalf("ListIdentityFingerprints returned error: %v", err)
	}
	if len(list) != 1 || list[0].AccountKey != accountKey {
		t.Fatalf("list = %#v, want one learned Claude account", list)
	}
	deleted, err := DeleteIdentityFingerprint(identityfingerprint.ProviderClaude, accountKey)
	if err != nil {
		t.Fatalf("DeleteIdentityFingerprint returned error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
}

func TestObserveIdentityFingerprintKeepsCodexAccountProductStable(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	accountKey := "codex-account"
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.130.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	headers.Set("Version", "0.130.0")
	headers.Set("Originator", "codex_cli_rs")
	headers.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	headers.Set("X-Codex-Beta-Features", "compact_mode")

	record, result, err := ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderCodex,
		AccountKey:    accountKey,
		AuthSubjectID: "subject-codex",
		Headers:       headers,
		ObservedAt:    time.Date(2026, 6, 23, 2, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint returned error: %v", err)
	}
	if result.Reason != "created" || record.ClientProduct != "codex_cli_rs" {
		t.Fatalf("merge result = %+v, product = %q, want codex_cli_rs created", result, record.ClientProduct)
	}
	if record.Fields[identityfingerprint.FieldCodexBetaFeatures] != "compact_mode" {
		t.Fatalf("X-Codex-Beta-Features = %q, want learned", record.Fields[identityfingerprint.FieldCodexBetaFeatures])
	}

	differentProductHeaders := http.Header{}
	differentProductHeaders.Set("User-Agent", "codex_other/9.0.0")
	differentProductHeaders.Set("Version", "9.0.0")
	differentProductHeaders.Set("Originator", "codex_other")
	record, result, err = ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderCodex,
		AccountKey:    accountKey,
		AuthSubjectID: "subject-codex",
		Headers:       differentProductHeaders,
		ObservedAt:    time.Date(2026, 6, 23, 3, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint different product returned error: %v", err)
	}
	if result.Reason != "different_product_last_seen" {
		t.Fatalf("different product reason = %q, want different_product_last_seen", result.Reason)
	}
	if record.ClientProduct != "codex_cli_rs" || record.Fields[identityfingerprint.FieldUserAgent] != headers.Get("User-Agent") {
		t.Fatalf("different product should not replace stable fields, got %#v", record)
	}
}

func TestObserveIdentityFingerprintLearnsGeminiCLIHeaders(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	headers := http.Header{}
	headers.Set("User-Agent", "google-api-nodejs-client/9.16.0")
	headers.Set("X-Goog-Api-Client", "gl-node/24.1.0")
	headers.Set("Client-Metadata", "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI")

	record, result, err := ObserveIdentityFingerprint(identityfingerprint.LearnInput{
		Provider:      identityfingerprint.ProviderGemini,
		AccountKey:    "gemini-account",
		AuthSubjectID: "subject-gemini",
		Headers:       headers,
		ObservedAt:    time.Date(2026, 6, 23, 4, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ObserveIdentityFingerprint returned error: %v", err)
	}
	if result.Reason != "created" || record.ClientProduct != "google-api-nodejs-client" || record.Version != "9.16.0" {
		t.Fatalf("record = %#v, result = %+v, want Gemini CLI created", record, result)
	}
	if record.Fields[identityfingerprint.FieldGeminiAPIClient] != "gl-node/24.1.0" {
		t.Fatalf("X-Goog-Api-Client = %q, want learned", record.Fields[identityfingerprint.FieldGeminiAPIClient])
	}
}

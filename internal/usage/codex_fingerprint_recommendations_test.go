package usage

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestQueryCodexFingerprintRecommendationsAggregatesStableHeaders(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	insertCodexFingerprintLog(t, now.Add(-2*time.Minute), map[string][]string{
		"User-Agent":              {"codex-tui/0.125.0 (Mac OS 26.5; arm64) Terminal.app/2.15 (codex-tui; 0.125.0)"},
		"Version":                 {"0.125.0"},
		"Originator":              {"codex-tui"},
		"OpenAI-Beta":             {"responses_websockets=2026-02-06"},
		"X-Codex-Beta-Features":   {"exec_command_v2"},
		"Session_id":              {"session-alpha-secret"},
		"X-Codex-Turn-Metadata":   {`{"turn":"alpha"}`},
		"Authorization":           {"Bearer should-not-be-exposed"},
		"X-Client-Request-Id":     {"req-alpha"},
		"X-Other-Non-Fingerprint": {"ignored"},
	})
	insertCodexFingerprintLog(t, now.Add(-time.Minute), map[string][]string{
		"User-Agent":            {"codex-tui/0.125.0 (Mac OS 26.5; arm64) Terminal.app/2.15 (codex-tui; 0.125.0)"},
		"Version":               {"0.125.0"},
		"Originator":            {"codex-tui"},
		"OpenAI-Beta":           {"responses_websockets=2026-02-06"},
		"X-Codex-Beta-Features": {"exec_command_v2"},
		"Session_id":            {"session-beta-secret"},
	})
	insertCodexFingerprintLog(t, now, map[string][]string{
		"User-Agent": {"curl/8.7.1"},
		"Version":    {"not-codex"},
	})

	result, err := QueryCodexFingerprintRecommendations(CodexFingerprintRecommendationQuery{
		Days:  7,
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("QueryCodexFingerprintRecommendations() error = %v", err)
	}

	if result.Inspected != 3 {
		t.Fatalf("Inspected = %d, want 3", result.Inspected)
	}
	if result.Matched != 2 {
		t.Fatalf("Matched = %d, want 2", result.Matched)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items length = %d, want 1", len(result.Items))
	}

	item := result.Items[0]
	if item.Count != 2 {
		t.Fatalf("Count = %d, want 2", item.Count)
	}
	if item.Recommended.UserAgent != "codex-tui/0.125.0 (Mac OS 26.5; arm64) Terminal.app/2.15 (codex-tui; 0.125.0)" {
		t.Fatalf("Recommended.UserAgent = %q", item.Recommended.UserAgent)
	}
	if item.Recommended.Version != "0.125.0" {
		t.Fatalf("Recommended.Version = %q, want 0.125.0", item.Recommended.Version)
	}
	if item.Recommended.Originator != "codex-tui" {
		t.Fatalf("Recommended.Originator = %q, want codex-tui", item.Recommended.Originator)
	}
	if item.Recommended.WebsocketBeta != "responses_websockets=2026-02-06" {
		t.Fatalf("Recommended.WebsocketBeta = %q", item.Recommended.WebsocketBeta)
	}
	if item.Recommended.SessionID != "" {
		t.Fatalf("Recommended.SessionID = %q, want empty", item.Recommended.SessionID)
	}
	if item.Recommended.BetaFeatures != "exec_command_v2" {
		t.Fatalf("Recommended.BetaFeatures = %q, want exec_command_v2", item.Recommended.BetaFeatures)
	}
	if _, exists := item.Recommended.CustomHeaders["X-Codex-Beta-Features"]; exists {
		t.Fatalf("X-Codex-Beta-Features must be recommended as a managed field, not a custom header")
	}
	if _, exists := item.Recommended.CustomHeaders["X-Codex-Turn-Metadata"]; exists {
		t.Fatalf("turn metadata must not be recommended as a fixed custom header")
	}
	if _, exists := item.Headers["Authorization"]; exists {
		t.Fatalf("Authorization should not be exposed in observed headers")
	}
	if got := item.IgnoredHeaders["Session_id"]; got == "" || got == "session-alpha-secret" || got == "session-beta-secret" {
		t.Fatalf("Session_id ignored header should be masked, got %q", got)
	}
	if got := item.IgnoredHeaders["X-Codex-Turn-Metadata"]; got == "" {
		t.Fatalf("X-Codex-Turn-Metadata should be visible as ignored sample metadata")
	}
	if len(item.Samples) != 2 {
		t.Fatalf("samples length = %d, want 2", len(item.Samples))
	}
}

func TestInitDBCreatesCodexFingerprintRecommendationDetailIndex(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	rows, err := getDB().Query("PRAGMA index_list(request_log_content)")
	if err != nil {
		t.Fatalf("PRAGMA index_list(request_log_content): %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == "idx_log_content_detail_timestamp" {
			found = true
			if partial != 1 {
				t.Fatalf("idx_log_content_detail_timestamp partial = %d, want 1", partial)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list: %v", err)
	}
	if !found {
		t.Fatal("idx_log_content_detail_timestamp was not created")
	}
}

func TestQueryCodexFingerprintRecommendationsSortsByCountThenRecent(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		insertCodexFingerprintLog(t, now.Add(time.Duration(i)*time.Second), map[string][]string{
			"User-Agent": {"codex-tui/0.124.0 (Mac OS 26.4; arm64)"},
			"Originator": {"codex-tui"},
		})
	}
	insertCodexFingerprintLog(t, now.Add(time.Minute), map[string][]string{
		"User-Agent": {"codex-tui/0.125.0 (Mac OS 26.5; arm64)"},
		"Originator": {"codex-tui"},
	})

	result, err := QueryCodexFingerprintRecommendations(CodexFingerprintRecommendationQuery{Days: 7, Limit: 20})
	if err != nil {
		t.Fatalf("QueryCodexFingerprintRecommendations() error = %v", err)
	}
	if len(result.Items) != 2 {
		t.Fatalf("items length = %d, want 2", len(result.Items))
	}
	if result.Items[0].Count != 2 {
		t.Fatalf("first item Count = %d, want 2", result.Items[0].Count)
	}
	if result.Items[1].Recommended.UserAgent != "codex-tui/0.125.0 (Mac OS 26.5; arm64)" {
		t.Fatalf("second item should be the newer one-count candidate, got %q", result.Items[1].Recommended.UserAgent)
	}
}

func insertCodexFingerprintLog(t *testing.T, timestamp time.Time, headers map[string][]string) {
	t.Helper()

	detail, err := json.Marshal(map[string]any{
		"client": map[string]any{
			"ip":                  "203.0.113.10",
			"method":              "POST",
			"path":                "/v1/responses",
			"host":                "relay.test",
			"fingerprint_headers": headers,
		},
	})
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}

	InsertLogWithDetails("sk-test", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, timestamp, 100, 10, TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "", string(detail))
}

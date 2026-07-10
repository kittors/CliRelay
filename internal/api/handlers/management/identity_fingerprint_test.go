package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestGetCodexFingerprintRecommendationsReturnsLogDerivedCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	detail, err := json.Marshal(map[string]any{
		"client": map[string]any{
			"ip":     "203.0.113.20",
			"method": "POST",
			"path":   "/v1/responses",
			"host":   "relay.test",
			"fingerprint_headers": map[string][]string{
				"User-Agent":            {"codex-tui/0.125.0 (Mac OS 26.5; arm64)"},
				"Version":               {"0.125.0"},
				"Originator":            {"codex-tui"},
				"X-Codex-Beta-Features": {"exec_command_v2"},
				"Session_id":            {"session-handler-secret"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	usage.InsertLogWithDetails("sk-test", "Primary", "gpt-5.4", "codex", "Codex", "auth-1", false, time.Now().UTC(), 100, 10, usage.TokenStats{
		InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
	}, "", "", string(detail))

	h := &Handler{cfg: &config.Config{}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/identity-fingerprint/codex/recommendations?days=7&limit=20", nil)

	h.GetCodexFingerprintRecommendations(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload struct {
		Items []struct {
			Count       int `json:"count"`
			Recommended struct {
				Enabled       bool              `json:"enabled"`
				UserAgent     string            `json:"user-agent"`
				Version       string            `json:"version"`
				Originator    string            `json:"originator"`
				BetaFeatures  string            `json:"x-codex-beta-features"`
				SessionID     string            `json:"session-id"`
				CustomHeaders map[string]string `json:"custom-headers"`
			} `json:"recommended"`
			IgnoredHeaders map[string]string `json:"ignored_headers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Items) != 1 {
		t.Fatalf("items length = %d, want 1", len(payload.Items))
	}
	item := payload.Items[0]
	if item.Count != 1 {
		t.Fatalf("Count = %d, want 1", item.Count)
	}
	if !item.Recommended.Enabled {
		t.Fatalf("recommended fingerprint should enable Codex identity")
	}
	if item.Recommended.UserAgent != "codex-tui/0.125.0 (Mac OS 26.5; arm64)" {
		t.Fatalf("user-agent = %q", item.Recommended.UserAgent)
	}
	if item.Recommended.Version != "0.125.0" {
		t.Fatalf("version = %q", item.Recommended.Version)
	}
	if item.Recommended.SessionID != "" {
		t.Fatalf("session-id = %q, want empty", item.Recommended.SessionID)
	}
	if item.Recommended.BetaFeatures != "exec_command_v2" {
		t.Fatalf("x-codex-beta-features = %q", item.Recommended.BetaFeatures)
	}
	if _, exists := item.Recommended.CustomHeaders["X-Codex-Beta-Features"]; exists {
		t.Fatalf("X-Codex-Beta-Features must be returned as a managed field, not a custom header")
	}
	if item.IgnoredHeaders["Session_id"] == "session-handler-secret" {
		t.Fatalf("session id must be masked in ignored headers")
	}
}

func TestListAuthFilesIncludesIdentityFingerprintSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	authPath := filepath.Join(t.TempDir(), "claude-oauth.json")
	if err := writeTestAuthFile(authPath); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	auth := &coreauth.Auth{
		ID:       "claude-oauth-1",
		FileName: "claude-oauth.json",
		Provider: "claude",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"email": "claude@example.com",
		},
	}
	identity := usage.ResolveAuthSubjectIdentity(auth)
	if identity == nil || identity.ID == "" {
		t.Fatal("expected auth subject identity")
	}
	if err := usage.UpsertIdentityFingerprint(&identityfingerprint.LearnedRecord{
		Provider:        identityfingerprint.ProviderClaude,
		AccountKey:      identity.ID,
		AuthSubjectID:   identity.ID,
		ClientProduct:   "claude-cli",
		Version:         "2.1.170",
		Fields:          map[string]string{identityfingerprint.FieldUserAgent: "claude-cli/2.1.170 (external, cli)"},
		ObservedHeaders: map[string]string{"User-Agent": "claude-cli/2.1.170 (external, cli)"},
		CreatedAt:       time.Date(2026, 6, 23, 1, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 6, 23, 1, 1, 0, 0, time.UTC),
		LastSeenAt:      time.Date(2026, 6, 23, 1, 2, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("UpsertIdentityFingerprint: %v", err)
	}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := &Handler{
		cfg: &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
			Claude: config.ClaudeIdentityFingerprintConfig{Enabled: true},
		}},
		authManager: manager,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)

	h.ListAuthFiles(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Files []struct {
			ID                         string `json:"id"`
			IdentityFingerprintSummary struct {
				Provider      string         `json:"provider"`
				AccountKey    string         `json:"account_key"`
				PrimarySource string         `json:"primary_source"`
				Learned       bool           `json:"learned"`
				LearnedFields int            `json:"learned_fields"`
				SourceCounts  map[string]int `json:"source_counts"`
				Version       string         `json:"version"`
			} `json:"identity_fingerprint_summary"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files length = %d, want 1", len(payload.Files))
	}
	summary := payload.Files[0].IdentityFingerprintSummary
	if summary.Provider != "claude" || summary.AccountKey != identity.ID {
		t.Fatalf("summary identity = %+v, want provider/account %s", summary, identity.ID)
	}
	if !summary.Learned || summary.PrimarySource != "learned" || summary.Version != "2.1.170" {
		t.Fatalf("summary learned/source/version = %+v", summary)
	}
	if summary.SourceCounts["learned"] != 2 || summary.SourceCounts["builtin_default"] == 0 {
		t.Fatalf("source counts = %#v, want learned and builtin_default fallback fields", summary.SourceCounts)
	}
}

func TestGetIdentityFingerprintAccountReturnsLearnedPresetAndBuiltinDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	accountKey := "authsub_codex_test"
	if err := usage.UpsertIdentityFingerprint(&identityfingerprint.LearnedRecord{
		Provider:      identityfingerprint.ProviderCodex,
		AccountKey:    accountKey,
		AuthSubjectID: accountKey,
		ClientProduct: "codex-tui",
		Version:       "0.125.0",
		Fields: map[string]string{
			identityfingerprint.FieldUserAgent:       "codex-tui/0.125.0 (Mac OS 26.5; arm64)",
			identityfingerprint.FieldCodexVersion:    "0.125.0",
			identityfingerprint.FieldCodexOriginator: "codex-tui",
		},
		ObservedHeaders: map[string]string{
			"User-Agent": "codex-tui/0.125.0 (Mac OS 26.5; arm64)",
			"Version":    "0.125.0",
			"Originator": "codex-tui",
		},
		CreatedAt:  time.Date(2026, 6, 23, 2, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 6, 23, 2, 1, 0, 0, time.UTC),
		LastSeenAt: time.Date(2026, 6, 23, 2, 2, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("UpsertIdentityFingerprint: %v", err)
	}

	h := &Handler{cfg: &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
		Codex: config.CodexIdentityFingerprintConfig{
			Enabled:       true,
			UserAgent:     "codex-tui/0.118.0 (Mac OS 26.3.1; arm64)",
			WebsocketBeta: "responses_websockets=2026-02-06",
		},
	}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/identity-fingerprint/account?provider=codex&account_key="+accountKey, nil)

	h.GetIdentityFingerprintAccount(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Summary struct {
			PrimarySource string         `json:"primary_source"`
			SourceCounts  map[string]int `json:"source_counts"`
			LearnedFields int            `json:"learned_fields"`
		} `json:"summary"`
		Effective struct {
			Fields map[string]struct {
				Value  string `json:"value"`
				Source string `json:"source"`
			} `json:"fields"`
		} `json:"effective"`
		Learned struct {
			ObservedHeaders map[string]string `json:"observed_headers"`
		} `json:"learned"`
		Preset struct {
			UserAgent string `json:"user-agent"`
		} `json:"preset"`
		BuiltinDefault struct {
			Originator string `json:"originator"`
		} `json:"builtin_default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got := payload.Effective.Fields[identityfingerprint.FieldUserAgent]; got.Value != "codex-tui/0.125.0 (Mac OS 26.5; arm64)" || got.Source != "learned" {
		t.Fatalf("user-agent field = %+v, want learned Codex client", got)
	}
	if got := payload.Effective.Fields[identityfingerprint.FieldCodexWebsocketBeta]; got.Value != "responses_websockets=2026-02-06" || got.Source != "preset" {
		t.Fatalf("websocket beta field = %+v, want preset fallback", got)
	}
	if payload.Summary.PrimarySource != "learned" || payload.Summary.LearnedFields != 3 {
		t.Fatalf("summary = %+v, want learned primary with three learned fields", payload.Summary)
	}
	if payload.Preset.UserAgent != "codex-tui/0.118.0 (Mac OS 26.3.1; arm64)" {
		t.Fatalf("preset user-agent = %q", payload.Preset.UserAgent)
	}
	if payload.BuiltinDefault.Originator != "codex-tui" {
		t.Fatalf("builtin default originator = %q", payload.BuiltinDefault.Originator)
	}
	if payload.Learned.ObservedHeaders["Version"] != "0.125.0" {
		t.Fatalf("observed headers = %#v", payload.Learned.ObservedHeaders)
	}
}

func TestGetIdentityFingerprintAccountReturnsXAIBuiltinDefaultFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)

	h := &Handler{cfg: &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
		XAI: config.XAIIdentityFingerprintConfig{Enabled: true},
	}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/identity-fingerprint/account?provider=xai&account_key=authsub_xai_test", nil)

	h.GetIdentityFingerprintAccount(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Summary struct {
			PrimarySource   string         `json:"primary_source"`
			SourceCounts    map[string]int `json:"source_counts"`
			EffectiveFields int            `json:"effective_fields"`
			Learned         bool           `json:"learned"`
			Version         string         `json:"version"`
		} `json:"summary"`
		Effective struct {
			Fields map[string]struct {
				Value  string `json:"value"`
				Source string `json:"source"`
			} `json:"fields"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload.Summary.Learned {
		t.Fatalf("summary learned = true, want false")
	}
	if payload.Summary.PrimarySource != "builtin_default" || payload.Summary.EffectiveFields != 3 {
		t.Fatalf("summary = %+v, want three builtin default fields", payload.Summary)
	}
	if payload.Summary.Version != config.DefaultXAIFingerprintClientVersion {
		t.Fatalf("summary version = %q, want %q", payload.Summary.Version, config.DefaultXAIFingerprintClientVersion)
	}
	if payload.Summary.SourceCounts["builtin_default"] != 3 {
		t.Fatalf("source counts = %#v, want three builtin default fields", payload.Summary.SourceCounts)
	}
	if got := payload.Effective.Fields[identityfingerprint.FieldUserAgent]; got.Value != config.DefaultXAIFingerprintUserAgent || got.Source != "builtin_default" {
		t.Fatalf("user-agent field = %+v, want xAI builtin default", got)
	}
	if got := payload.Effective.Fields[identityfingerprint.FieldXAIClientIdentifier]; got.Value != config.DefaultXAIFingerprintClientIdentifier || got.Source != "builtin_default" {
		t.Fatalf("client identifier field = %+v, want xAI builtin default", got)
	}
	if got := payload.Effective.Fields[identityfingerprint.FieldXAIClientVersion]; got.Value != config.DefaultXAIFingerprintClientVersion || got.Source != "builtin_default" {
		t.Fatalf("client version field = %+v, want xAI builtin default", got)
	}
}

func TestBuildIdentityFingerprintSummaryUsesCodexPresetVersionWhenLearnedVersionMissing(t *testing.T) {
	learned := &identityfingerprint.LearnedRecord{
		Provider:      identityfingerprint.ProviderCodex,
		AccountKey:    "authsub_codex_desktop",
		AuthSubjectID: "authsub_codex_desktop",
		ClientProduct: "codex",
		ClientVariant: "Codex Desktop",
		Fields: map[string]string{
			identityfingerprint.FieldUserAgent:       "Codex Desktop/0.142.0 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.616.81150)",
			identityfingerprint.FieldCodexOriginator: "Codex Desktop",
		},
	}
	_, effective := identityfingerprint.ResolveCodex(config.CodexIdentityFingerprintConfig{
		Enabled: true,
		Version: "0.140.0",
	}, learned)

	summary := buildIdentityFingerprintSummary(
		identityfingerprint.ProviderCodex,
		"authsub_codex_desktop",
		"authsub_codex_desktop",
		learned,
		effective,
	)

	if summary.Version != "0.140.0" {
		t.Fatalf("summary version = %q, want preset version fallback", summary.Version)
	}
}

func TestBuildIdentityFingerprintSummaryUsesClaudeLearnedCLIVersion(t *testing.T) {
	learned := &identityfingerprint.LearnedRecord{
		Provider:      identityfingerprint.ProviderClaude,
		AccountKey:    "authsub_claude_cli",
		AuthSubjectID: "authsub_claude_cli",
		ClientProduct: "claude-cli",
		Fields: map[string]string{
			identityfingerprint.FieldClaudeCLIVersion: "2.1.170",
			identityfingerprint.FieldUserAgent:        "claude-cli/2.1.170 (external, cli)",
		},
	}
	_, effective := identityfingerprint.ResolveClaude(config.ClaudeIdentityFingerprintConfig{Enabled: true}, learned)

	summary := buildIdentityFingerprintSummary(
		identityfingerprint.ProviderClaude,
		"authsub_claude_cli",
		"authsub_claude_cli",
		learned,
		effective,
	)

	if summary.Version != "2.1.170" {
		t.Fatalf("summary version = %q, want learned cli-version", summary.Version)
	}
}

func writeTestAuthFile(path string) error {
	return os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600)
}

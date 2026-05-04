package management

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func TestPutIdentityFingerprintPersistsToSQLite(t *testing.T) {
	initManagementModelsTestDB(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("identity-fingerprint:\n  codex:\n    enabled: false\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(&config.Config{LoggingToFile: true}, configPath, nil)

	rec := performModelsRequest(http.MethodPut, "/identity-fingerprint", []byte(`{
		"codex": {
			"enabled": true,
			"user-agent": "codex_cli_rs/0.125.0",
			"originator": "codex_cli_rs",
			"session-mode": "per-request"
		},
		"claude": {
			"enabled": true,
			"cli-version": "2.1.88",
			"entrypoint": "cli",
			"session-mode": "server-stable"
		}
	}`), h.PutIdentityFingerprint)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutIdentityFingerprint status = %d body = %s", rec.Code, rec.Body.String())
	}

	var stored config.Config
	if !usage.ApplyStoredRuntimeSettings(&stored) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if !stored.IdentityFingerprint.Codex.Enabled || stored.IdentityFingerprint.Codex.UserAgent != "codex_cli_rs/0.125.0" {
		t.Fatalf("stored identity fingerprint = %#v", stored.IdentityFingerprint.Codex)
	}
	if !stored.IdentityFingerprint.Claude.Enabled || stored.IdentityFingerprint.Claude.UserAgent != "claude-cli/2.1.88 (external, cli)" ||
		stored.IdentityFingerprint.Claude.SessionMode != "server-stable" {
		t.Fatalf("stored claude identity fingerprint = %#v", stored.IdentityFingerprint.Claude)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "identity-fingerprint:") {
		t.Fatalf("identity-fingerprint should be removed from YAML after DB write:\n%s", string(data))
	}
	if !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
}

func TestPutOAuthModelAliasPersistsToSQLite(t *testing.T) {
	initManagementModelsTestDB(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("oauth-model-alias:\n  codex:\n    - name: old\n      alias: stale\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(&config.Config{LoggingToFile: true}, configPath, nil)

	rec := performModelsRequest(http.MethodPut, "/oauth-model-alias", []byte(`{
		"codex": [
			{"name": "gpt-5.3-codex", "alias": "codex-latest", "fork": true}
		]
	}`), h.PutOAuthModelAlias)
	if rec.Code != http.StatusOK {
		t.Fatalf("PutOAuthModelAlias status = %d body = %s", rec.Code, rec.Body.String())
	}

	var stored config.Config
	if !usage.ApplyStoredRuntimeSettings(&stored) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	aliases := stored.OAuthModelAlias["codex"]
	if len(aliases) != 1 || aliases[0].Name != "gpt-5.3-codex" || aliases[0].Alias != "codex-latest" || !aliases[0].Fork {
		t.Fatalf("stored oauth-model-alias = %#v", stored.OAuthModelAlias)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "oauth-model-alias:") {
		t.Fatalf("oauth-model-alias should be removed from YAML after DB write:\n%s", string(data))
	}
	if !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
}

func TestPutProviderCredentialsPersistToSQLite(t *testing.T) {
	initManagementModelsTestDB(t)

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("codex-api-key:\n  - api-key: old-codex\n    base-url: https://old.example.com\nclaude-api-key:\n  - api-key: old-claude\n    base-url: https://old.example.com\nlogging-to-file: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	h := NewHandler(&config.Config{LoggingToFile: true}, configPath, nil)

	codexRec := performModelsRequest(http.MethodPut, "/codex-api-key", []byte(`[
		{"api-key": "sk-codex-db", "base-url": "https://codex.example.com"}
	]`), h.PutCodexKeys)
	if codexRec.Code != http.StatusOK {
		t.Fatalf("PutCodexKeys status = %d body = %s", codexRec.Code, codexRec.Body.String())
	}
	claudeRec := performModelsRequest(http.MethodPut, "/claude-api-key", []byte(`[
		{"api-key": "sk-claude-db", "name": "claude-db", "base-url": "https://claude.example.com"}
	]`), h.PutClaudeKeys)
	if claudeRec.Code != http.StatusOK {
		t.Fatalf("PutClaudeKeys status = %d body = %s", claudeRec.Code, claudeRec.Body.String())
	}

	var stored config.Config
	if !usage.ApplyStoredRuntimeSettings(&stored) {
		t.Fatal("ApplyStoredRuntimeSettings returned false")
	}
	if len(stored.CodexKey) != 1 || stored.CodexKey[0].APIKey != "sk-codex-db" || stored.CodexKey[0].BaseURL != "https://codex.example.com" {
		t.Fatalf("stored codex keys = %#v", stored.CodexKey)
	}
	if len(stored.ClaudeKey) != 1 || stored.ClaudeKey[0].APIKey != "sk-claude-db" || stored.ClaudeKey[0].Name != "claude-db" {
		t.Fatalf("stored claude keys = %#v", stored.ClaudeKey)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, forbidden := range []string{"codex-api-key:", "claude-api-key:"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("%s should be removed from YAML after DB write:\n%s", forbidden, string(data))
		}
	}
	if !strings.Contains(string(data), "logging-to-file: true") {
		t.Fatalf("ordinary config should remain in YAML:\n%s", string(data))
	}
}

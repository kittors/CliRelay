package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexUpstreamIncrementalMode(t *testing.T) {
	var nilConfig *Config
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"", CodexUpstreamIncrementalOff},
		{"off", CodexUpstreamIncrementalOff},
		{"Shadow", CodexUpstreamIncrementalShadow},
		{" on ", CodexUpstreamIncrementalOn},
		{"yes", CodexUpstreamIncrementalOff},
	} {
		cfg := &Config{CodexUpstreamIncremental: CodexUpstreamIncrementalConfig{Mode: tc.mode}}
		if got := cfg.CodexUpstreamIncrementalMode(); got != tc.want {
			t.Fatalf("mode %q = %q, want %q", tc.mode, got, tc.want)
		}
	}
	if got := nilConfig.CodexUpstreamIncrementalMode(); got != CodexUpstreamIncrementalOff {
		t.Fatalf("nil config mode = %q", got)
	}
}

func TestCodexUpstreamIncrementalAllowsKey(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-allowed"))
	cfg := &Config{CodexUpstreamIncremental: CodexUpstreamIncrementalConfig{AllowKeys: []string{"sha256:" + strings.ToUpper(hex.EncodeToString(sum[:])), "sk-plain"}}}
	for key, want := range map[string]bool{"sk-allowed": true, "sk-plain": true, "sk-other": false, "": false} {
		if got := cfg.CodexUpstreamIncrementalAllowsKey(key); got != want {
			t.Fatalf("AllowsKey(%q) = %t, want %t", key, got, want)
		}
	}
	if !(&Config{}).CodexUpstreamIncrementalAllowsKey("anything") {
		t.Fatal("an empty list must allow every key")
	}
}

func TestCodexUpstreamIncrementalLimitsDefault(t *testing.T) {
	cfg := &Config{}
	if cfg.CodexUpstreamIncrementalMaxFirstRequestBytes() != 15<<20 || cfg.CodexUpstreamIncrementalMaxChains() != 256 ||
		cfg.CodexUpstreamIncrementalMaxChainsPerAuth() != 16 || cfg.CodexUpstreamIncrementalIdleTimeout() != 5*time.Minute {
		t.Fatal("unexpected defaults")
	}
}

// The block is written by hand into config.yaml and has to survive a load and
// a management save, list included.
func TestCodexUpstreamIncrementalSurvivesYAMLSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "# operator config\ncodex-upstream-incremental:\n  mode: on\n  allow-keys:\n    - sha256:abc\n  max-first-request-mb: 20\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.CodexUpstreamIncremental
	if restored.CodexUpstreamIncrementalMode() != CodexUpstreamIncrementalOn || len(got.AllowKeys) != 1 || got.AllowKeys[0] != "sha256:abc" || got.MaxFirstRequestMB != 20 {
		t.Fatalf("after save and reload: %+v", got)
	}

	unset := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(unset, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(unset)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveComments(unset, cfg); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(unset)
	if strings.Contains(string(saved), "codex-upstream-incremental") {
		t.Fatalf("an unset block was written back:\n%s", saved)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRequestZstdEnabledDefaultsOn(t *testing.T) {
	on, off := true, false
	var nilConfig *Config
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config", nilConfig, true},
		{"unset", &Config{}, true},
		{"explicitly on", &Config{CodexRequestZstd: &on}, true},
		{"explicitly off", &Config{CodexRequestZstd: &off}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CodexRequestZstdEnabled(); got != tc.want {
				t.Fatalf("CodexRequestZstdEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An explicit false has to survive a load and a management save, and an unset
// key must stay unset instead of turning into an explicit choice.
func TestCodexRequestZstdSurvivesYAMLSave(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		want        bool
		wantWritten string // text the saved file must hold; empty means the key must be absent
	}{
		{"unset", "# operator config\nport: 8317\n", true, ""},
		{"off", "# operator config\ncodex-request-zstd: false\n", false, "codex-request-zstd: false"},
		{"on", "# operator config\ncodex-request-zstd: true\n", true, "codex-request-zstd: true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.CodexRequestZstdEnabled(); got != tc.want {
				t.Fatalf("after load: enabled = %v, want %v", got, tc.want)
			}
			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWritten == "" && strings.Contains(string(saved), "codex-request-zstd") {
				t.Fatalf("an unset key was written back:\n%s", saved)
			}
			if tc.wantWritten != "" && !strings.Contains(string(saved), tc.wantWritten) {
				t.Fatalf("saved file lost %q:\n%s", tc.wantWritten, saved)
			}
			restored, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := restored.CodexRequestZstdEnabled(); got != tc.want {
				t.Fatalf("after save and reload: enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

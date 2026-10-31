package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// streaming.bootstrap-retries distinguishes "not configured" (take the
// default, which is on) from an explicit 0 (off). The difference has to
// survive a load, a management save that rewrites config.yaml, a reload, and
// the JSON the management API returns; an int with omitempty lost the explicit
// 0 at every one of those steps.
func TestStreamingBootstrapRetriesKeepsUnsetApartFromZero(t *testing.T) {
	cases := []struct {
		name        string
		yaml        string
		want        *int
		wantWritten string // text the saved file must hold; empty means the key must be absent
		wantJSON    string // fragment of the management JSON; empty means the key must be absent
	}{
		{"unset", "port: 8317\nstreaming:\n  keepalive-seconds: 20\n", nil, "", ""},
		{"explicit zero", "port: 8317\nstreaming:\n  bootstrap-retries: 0\n", new(0), "bootstrap-retries: 0", `"bootstrap-retries":0`},
		{"explicit value", "port: 8317\nstreaming:\n  bootstrap-retries: 3\n", new(3), "bootstrap-retries: 3", `"bootstrap-retries":3`},
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
			assertOptionalInt(t, "after load", cfg.Streaming.BootstrapRetries, tc.want)

			encoded, err := json.Marshal(cfg.Streaming)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantJSON == "" && strings.Contains(string(encoded), "bootstrap-retries") {
				t.Fatalf("management JSON reports an unset value: %s", encoded)
			}
			if tc.wantJSON != "" && !strings.Contains(string(encoded), tc.wantJSON) {
				t.Fatalf("management JSON %s lost %s", encoded, tc.wantJSON)
			}

			if err := SaveConfigPreserveComments(path, cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWritten == "" && strings.Contains(string(saved), "bootstrap-retries") {
				t.Fatalf("an unset key was written back:\n%s", saved)
			}
			if tc.wantWritten != "" && !strings.Contains(string(saved), tc.wantWritten) {
				t.Fatalf("saved file lost %q:\n%s", tc.wantWritten, saved)
			}
			restored, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			assertOptionalInt(t, "after save and reload", restored.Streaming.BootstrapRetries, tc.want)

			var decoded StreamingConfig
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			assertOptionalInt(t, "after a JSON round trip", decoded.BootstrapRetries, tc.want)
		})
	}
}

func assertOptionalInt(t *testing.T, stage string, got, want *int) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s: got %d, want unset", stage, *got)
	case want != nil && got == nil:
		t.Fatalf("%s: got unset, want %d", stage, *want)
	case want != nil && *got != *want:
		t.Fatalf("%s: got %d, want %d", stage, *got, *want)
	}
}

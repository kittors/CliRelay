package diff

import (
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// The switch changes what goes out on the wire, so the reload log names it, but
// only when the effective value moves: unset and explicitly on are the same.
func TestBuildConfigChangeDetailsReportsCodexRequestZstd(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name   string
		oldCfg *config.Config
		newCfg *config.Config
		want   string // empty: no entry expected
	}{
		{"unset to off", &config.Config{}, &config.Config{CodexRequestZstd: &off}, "codex-request-zstd: true -> false"},
		{"on to off", &config.Config{CodexRequestZstd: &on}, &config.Config{CodexRequestZstd: &off}, "codex-request-zstd: true -> false"},
		{"off to unset", &config.Config{CodexRequestZstd: &off}, &config.Config{}, "codex-request-zstd: false -> true"},
		{"unset to explicitly on", &config.Config{}, &config.Config{CodexRequestZstd: &on}, ""},
		{"unchanged off", &config.Config{CodexRequestZstd: &off}, &config.Config{CodexRequestZstd: &off}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			details := BuildConfigChangeDetails(tc.oldCfg, tc.newCfg)
			if tc.want != "" && !slices.Contains(details, tc.want) {
				t.Fatalf("details %v do not contain %q", details, tc.want)
			}
			if tc.want == "" {
				for _, detail := range details {
					if strings.Contains(detail, "codex-request-zstd") {
						t.Fatalf("unexpected entry %q", detail)
					}
				}
			}
		})
	}
}

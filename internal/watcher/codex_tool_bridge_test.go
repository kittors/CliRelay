package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/diff"
)

// Toggling codex-tool-bridge in config.yaml must reach the running credential
// without a restart: the same auth is modified in place, with the attribute
// the executor reads added or removed.
func TestReloadCodexToolBridgeOptIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config func(authDir, field string) string
		// diffLine is the change summary logged on reload, where the provider
		// kind reports field-level changes at all.
		diffLine func(enabled bool) string
	}{
		{
			name: "openai-compatibility",
			config: func(authDir, field string) string {
				return fmt.Sprintf("auth-dir: %s\nopenai-compatibility:\n  - name: bridged\n    base-url: https://bridge.example/v1\n    api-key-entries:\n      - api-key: test-key\n%s", authDir, field)
			},
			diffLine: func(enabled bool) string {
				return fmt.Sprintf("codex-tool-bridge %t -> %t", !enabled, enabled)
			},
		},
		{
			name: "opencode-go",
			config: func(authDir, field string) string {
				return fmt.Sprintf("auth-dir: %s\nopencode-go-api-key:\n  - api-key: test-go\n%s", authDir, field)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := &Watcher{configPath: filepath.Join(dir, "config.yaml"), authDir: dir, mirroredAuthDir: dir}
			w.SetConfig(&config.Config{AuthDir: dir})
			queue := make(chan AuthUpdate, 4)
			w.SetAuthUpdateQueue(queue)
			t.Cleanup(func() { w.SetAuthUpdateQueue(nil) })
			var authID string
			for i, field := range []string{"", "    codex-tool-bridge: true\n", "    codex-tool-bridge: false\n"} {
				enabled := i == 1
				previous := w.config
				if err := os.WriteFile(w.configPath, []byte(tc.config(dir, field)), 0o600); err != nil {
					t.Fatal(err)
				}
				if !w.reloadConfig() {
					t.Fatal("reloadConfig failed")
				}
				select {
				case update := <-queue:
					if update.Auth == nil {
						t.Fatalf("step %d: expected an auth, got %#v", i, update)
					}
					if i == 0 {
						authID = update.Auth.ID
					} else if update.Action != AuthUpdateActionModify || update.Auth.ID != authID {
						t.Fatalf("step %d: expected the same credential to be modified, got %#v", i, update)
					}
					if got := update.Auth.Attributes["codex_tool_bridge"]; (got == "true") != enabled {
						t.Fatalf("step %d: codex_tool_bridge = %q, want enabled=%t", i, got, enabled)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("step %d: no auth update after toggling codex-tool-bridge", i)
				}
				if i > 0 && tc.diffLine != nil {
					changes := strings.Join(diff.BuildConfigChangeDetails(previous, w.config), "\n")
					if !strings.Contains(changes, tc.diffLine(enabled)) {
						t.Fatalf("step %d: reload summary does not mention %q:\n%s", i, tc.diffLine(enabled), changes)
					}
				}
			}
		})
	}
}

package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/diff"
)

func TestReloadCodexAlphaSearchOptIn(t *testing.T) {
	dir := t.TempDir()
	w := &Watcher{configPath: filepath.Join(dir, "config.yaml"), authDir: dir, mirroredAuthDir: dir}
	w.SetConfig(&config.Config{AuthDir: dir})
	queue := make(chan AuthUpdate, 4)
	w.SetAuthUpdateQueue(queue)
	t.Cleanup(func() { w.SetAuthUpdateQueue(nil) })
	var authID string
	for i, field := range []string{"", "    alpha-search: true\n", "    alpha-search: false\n"} {
		previous := w.config
		data := fmt.Sprintf("auth-dir: %s\ncodex-api-key:\n  - api-key: test-codex\n%s", dir, field)
		if err := os.WriteFile(w.configPath, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if !w.reloadConfig() {
			t.Fatal("reloadConfig failed")
		}
		select {
		case update := <-queue:
			if update.Auth == nil {
				t.Fatalf("expected auth, got %#v", update)
			}
			if i == 0 {
				authID = update.Auth.ID
			} else if update.Action != AuthUpdateActionModify || update.Auth.ID != authID {
				t.Fatalf("expected same credential modify, got %#v", update)
			}
			if got := update.Auth.Attributes["alpha_search"]; (got == "true") != (i == 1) {
				t.Fatalf("step %d: alpha_search = %q", i, got)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("step %d: no auth update on capability change", i)
		}
		if i > 0 {
			changes := strings.Join(diff.BuildConfigChangeDetails(previous, w.config), "\n")
			if !strings.Contains(changes, "codex[0].alpha-search:") {
				t.Fatalf("capability toggle missing from diff: %s", changes)
			}
		}
	}
}

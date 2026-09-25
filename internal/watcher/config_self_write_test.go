package watcher

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// A management save rewrites config.yaml and applies the change itself; the
// watcher used to reload the same content a second time, rebuilding every
// executor. It must skip that echo but still reload an operator's edit.
func TestReloadSkipsConfigWrittenByThisProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8318\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloads atomic.Int32
	w, err := NewWatcher(path, filepath.Join(dir, "auth"), func(*config.Config) { reloads.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Stop() }()
	w.SetConfig(&config.Config{})

	if err := config.WriteYAMLFileAtomic(path, []byte("port: 8318\ndebug: true\n")); err != nil {
		t.Fatal(err)
	}
	w.reloadConfigIfChanged()
	if got := reloads.Load(); got != 0 {
		t.Fatalf("reloads after own write = %d, want 0", got)
	}

	if err := os.WriteFile(path, []byte("port: 8319\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.reloadConfigIfChanged()
	if got := reloads.Load(); got != 1 {
		t.Fatalf("reloads after external edit = %d, want 1", got)
	}
}

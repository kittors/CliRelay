package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// atomicReplace mirrors config.WriteYAMLFileAtomic: write a temp file in the same
// directory, then rename it over the target.
func atomicReplace(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	tmpPath := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		t.Fatalf("write temp: %v", err)
	}
	if err = tmp.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		t.Fatalf("rename: %v", err)
	}
}

// startConfigWatcher runs the production event loop against a real fsnotify watch on the
// config file, counting reloads through the normal reload callback.
func startConfigWatcher(t *testing.T) (configPath string, reloads *atomic.Int32) {
	t.Helper()
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath = filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth_dir: "+authDir+"\nport: 8317\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new fsnotify watcher: %v", err)
	}
	if err = fsWatcher.Add(configPath); err != nil {
		t.Fatalf("watch config: %v", err)
	}

	var counter atomic.Int32
	w := &Watcher{
		authDir:        authDir,
		configPath:     configPath,
		watcher:        fsWatcher,
		lastAuthHashes: make(map[string]string),
		reloadCallback: func(*config.Config) { counter.Add(1) },
	}
	w.SetConfig(&config.Config{AuthDir: authDir})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = fsWatcher.Close()
	})
	go w.processEvents(ctx)

	return configPath, &counter
}

func waitForReloads(t *testing.T, reloads *atomic.Int32, want int32, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reloads.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: reloads = %d, want >= %d", msg, reloads.Load(), want)
}

// Regression for the atomic-write rollout: config.yaml is now published via rename, and
// inotify watches inodes rather than paths. The replaced inode's watch is dropped
// silently — IN_IGNORED, nothing on the Errors channel — so unless handleEvent re-arms it,
// the first atomic save permanently disables config hot-reload.
//
// The second and third replaces are the assertions that matter; the first is still seen on
// the soon-to-die watch. This reproduces only on inotify: kqueue (macOS) re-adds the watch
// internally, so on macOS it passes either way and Linux CI is what guards the regression.
// Each replace writes distinct content so the reload path's content-hash check lets it
// through.
func TestConfigWatchSurvivesRepeatedAtomicReplace(t *testing.T) {
	configPath, reloads := startConfigWatcher(t)
	authDir := filepath.Join(filepath.Dir(configPath), "auth")

	for i := 1; i <= 3; i++ {
		atomicReplace(t, configPath, fmt.Appendf(nil, "auth_dir: %s\nport: %d\n", authDir, 8317+i))
		waitForReloads(t, reloads, int32(i), fmt.Sprintf("atomic replace #%d", i))
	}
}

// After an atomic replace the watch must also still notice an ordinary in-place write —
// an operator editing config.yaml by hand, and the EBUSY in-place fallback.
func TestConfigWatchNoticesInPlaceWriteAfterAtomicReplace(t *testing.T) {
	configPath, reloads := startConfigWatcher(t)
	authDir := filepath.Join(filepath.Dir(configPath), "auth")

	atomicReplace(t, configPath, fmt.Appendf(nil, "auth_dir: %s\nport: 8318\n", authDir))
	waitForReloads(t, reloads, 1, "atomic replace")

	if err := os.WriteFile(configPath, fmt.Appendf(nil, "auth_dir: %s\nport: 8319\n", authDir), 0o600); err != nil {
		t.Fatalf("in-place write: %v", err)
	}
	waitForReloads(t, reloads, 2, "in-place write after atomic replace")
}

// handleEvent must treat a rename-away as a content change. On inotify the replaced inode
// reports Remove (never Write or Create), so dropping Remove would skip the reload even
// while the watch itself stays healthy.
func TestHandleEventSchedulesReloadForContentOps(t *testing.T) {
	for _, op := range []fsnotify.Op{fsnotify.Remove, fsnotify.Rename, fsnotify.Write, fsnotify.Create} {
		t.Run(op.String(), func(t *testing.T) {
			w, configPath := newHandleEventWatcher(t)
			w.handleEvent(fsnotify.Event{Name: configPath, Op: op})
			if timer := takeReloadTimer(w); timer == nil {
				t.Fatalf("%s on config path did not schedule a reload", op)
			}
		})
	}
}

// A bare permission change re-arms the watch but is not a content change: reloading on it
// would double every atomic replace, which reports Chmod alongside Remove.
func TestHandleEventDoesNotScheduleReloadForChmod(t *testing.T) {
	w, configPath := newHandleEventWatcher(t)
	w.handleEvent(fsnotify.Event{Name: configPath, Op: fsnotify.Chmod})
	if timer := takeReloadTimer(w); timer != nil {
		t.Fatal("chmod on config path scheduled a reload")
	}
}

func newHandleEventWatcher(t *testing.T) (*Watcher, string) {
	t.Helper()
	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatalf("create auth dir: %v", err)
	}
	configPath := filepath.Join(tmpDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth_dir: "+authDir+"\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("new fsnotify watcher: %v", err)
	}
	t.Cleanup(func() { _ = fsWatcher.Close() })
	if err = fsWatcher.Add(configPath); err != nil {
		t.Fatalf("watch config: %v", err)
	}
	return &Watcher{
		authDir:        authDir,
		configPath:     configPath,
		watcher:        fsWatcher,
		lastAuthHashes: make(map[string]string),
	}, configPath
}

// takeReloadTimer returns and disarms any pending reload timer, so the test never lets a
// real reload run.
func takeReloadTimer(w *Watcher) *time.Timer {
	w.configReloadMu.Lock()
	defer w.configReloadMu.Unlock()
	timer := w.configReloadTimer
	if timer != nil {
		timer.Stop()
		w.configReloadTimer = nil
	}
	return timer
}

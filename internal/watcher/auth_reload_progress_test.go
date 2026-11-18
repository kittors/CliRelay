package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestAuthReloadMakesProgressDuringContinuousWrites(t *testing.T) {
	// Virtual time makes the sustained event cadence deterministic, without
	// depending on scheduler pauses or shortening the production debounce.
	synctest.Test(t, func(t *testing.T) {
		authDir := t.TempDir()
		var reloads atomic.Int32
		w := &Watcher{
			authDir:        authDir,
			lastAuthHashes: make(map[string]string),
			reloadCallback: func(*config.Config) { reloads.Add(1) },
		}
		w.SetConfig(&config.Config{AuthDir: authDir})
		defer w.stopAuthReloadTimer()

		started := time.Now()
		for i := 0; i < 20; i++ {
			path := filepath.Join(authDir, fmt.Sprintf("account-%d.json", i%3))
			body := []byte(fmt.Sprintf(`{"type":"demo","label":"revision-%d"}`, i))
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			w.scheduleAuthFileUpdate(path)
			time.Sleep(100 * time.Millisecond)
			synctest.Wait()
		}
		duringWrites := reloads.Load()
		w.authReloadMu.Lock()
		pendingDuringWrites := len(w.pendingAuthPaths)
		w.authReloadMu.Unlock()
		t.Logf("continuous writes: elapsed=%s callbacks=%d pending_files=%d", time.Since(started), duringWrites, pendingDuringWrites)

		// Let the actual AfterFunc run through hash checks and addOrUpdateClient;
		// the callback after silence proves that the fixture is functional.
		time.Sleep(authReloadDebounce + time.Millisecond)
		synctest.Wait()
		afterSilence := reloads.Load()
		t.Logf("after silence: callbacks=%d", afterSilence)
		if afterSilence == 0 {
			t.Fatal("fixture never reached the real auth reload callback")
		}
		if duringWrites == 0 {
			t.Fatalf("all auth updates starved for 2s of writes despite %s debounce; pending=%d, callbacks after silence=%d", authReloadDebounce, pendingDuringWrites, afterSilence)
		}
	})
}

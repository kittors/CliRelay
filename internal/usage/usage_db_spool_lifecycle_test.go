package usage

import (
	"database/sql/driver"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// A direct writer is not covered by the SDK dispatcher barrier. Its store
// lease must therefore include fallback append, but not application callbacks.
// Lock the real spool rather than substituting a standalone persistence path.
func TestCloseDBWaitsForDirectSpoolAppendButNotTokenHook(t *testing.T) {
	for _, down := range []bool{true, false} {
		name := "transient"
		if down {
			name = "health_down"
		}
		t.Run(name, func(t *testing.T) {
			initTestUsageDB(t, config.RequestLogStorageConfig{StoreContent: true})
			resetUsageWriteStateForTest(t)
			spool := useUsageSpoolForTest(t, "", 0)
			usageWriteShuttingDown.Store(true) // One real write attempt, without retry sleeps.
			var attempts atomic.Int32
			writeRequestLogAttemptFunc = func(RequestLogEntry, time.Duration) (requestLogWriteResult, error) {
				attempts.Add(1)
				return requestLogWriteResult{}, driver.ErrBadConn
			}
			if down {
				usageDBHealth.markDown(driver.ErrBadConn)
			}
			hookEntered, hookRelease := make(chan struct{}), make(chan struct{})
			written, closed := make(chan struct{}), make(chan struct{})
			var hooks atomic.Int32
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(hookRelease) }) }
			SetTokenUsageCallback(func(_ string, _ string, tokens int64) {
				if tokens != 9 {
					t.Errorf("token hook got %d, want 9", tokens)
				}
				hooks.Add(1)
				close(hookEntered)
				<-hookRelease
			})
			spool.mu.Lock()
			var unlockOnce sync.Once
			unlock := func() { unlockOnce.Do(spool.mu.Unlock) }
			// Unblock all fixtures and join the writer before restoring package globals,
			// including when an assertion fails on the pre-fix implementation.
			defer func() { unlock(); release(); awaitUsageLifecycle(t, written, "direct writer cleanup") }()
			go func() {
				InsertRequestLog(RequestLogEntry{Model: "blocked-spool", Timestamp: time.Now(), Tokens: TokenStats{TotalTokens: 9}})
				close(written)
			}()
			// No maintenance or replayer is running. The writer is the only possible
			// reader; observing its lease makes this independent of goroutine launch timing.
			deadline := time.Now().Add(5 * time.Second)
			for usageWriteLease.TryLock() {
				usageWriteLease.Unlock()
				if time.Now().After(deadline) {
					t.Fatal("writer never held the store lease while append was blocked")
				}
				time.Sleep(time.Millisecond)
			}
			go func() { CloseDB(); close(closed) }()
			defer func() {
				unlock()
				release()
				awaitUsageLifecycle(t, closed, "CloseDB cleanup")
			}()
			// Keep spool locked and the writer admitted: Close cannot detach this store.
			select {
			case <-closed:
				t.Fatal("CloseDB returned before direct fallback append")
			case <-time.After(20 * time.Millisecond):
			}
			if spool.appended.Load() != 0 || hooks.Load() != 0 {
				t.Fatal("append or hook passed the spool mutex")
			}
			unlock()
			awaitUsageLifecycle(t, hookEntered, "post-append token hook")
			awaitUsageLifecycle(t, closed, "CloseDB while external hook remains blocked")
			if n, _, _ := spool.pending(); n != 1 {
				t.Errorf("pending records=%d, want exactly 1", n)
			}
			if spool.appended.Load() != 1 || hooks.Load() != 1 {
				t.Errorf("append=%d hook=%d, want one each", spool.appended.Load(), hooks.Load())
			}
			wantAttempts := int32(1)
			if down {
				wantAttempts = 0
			}
			if attempts.Load() != wantAttempts {
				t.Errorf("DB attempts=%d want %d", attempts.Load(), wantAttempts)
			}
			select {
			case <-written:
				t.Error("writer finished despite blocked external hook")
			default:
			}
			release()
			awaitUsageLifecycle(t, written, "direct writer after hook release")
		})
	}
}

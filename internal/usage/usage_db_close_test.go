package usage

import (
	"context"
	"testing"
	"time"
)

func TestCloseDBStopsMaintenanceBeforeLockingUsageDB(t *testing.T) {
	CloseDB()

	ctx, cancel := context.WithCancel(context.Background())
	requestLogMaintenanceCancel = cancel
	requestLogMaintenanceWG.Add(1)
	go func() {
		defer requestLogMaintenanceWG.Done()
		<-ctx.Done()
		usageDBMu.Lock()
		_ = usageLoc
		usageDBMu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		CloseDB()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseDB deadlocked while maintenance waited for usageDBMu")
	}
}

package usage

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type quiescePluginFunc func(context.Context, Record)

func (f quiescePluginFunc) HandleUsage(ctx context.Context, r Record) { f(ctx, r) }
func quiesceWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
func quiesceMustWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s completed before its dependency", what)
	case <-time.After(20 * time.Millisecond):
	}
}
func quiesceUnblockOnCleanup(t *testing.T, release chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
}

func quiesceStop(t *testing.T, m *Manager) {
	t.Helper()
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !m.Wait(ctx) {
		t.Error("usage dispatcher did not stop")
	}
}

// Queue length alone is not a drain: the first record is already in a plugin,
// the second is queued, and the third publisher is waiting for queue capacity.
func TestManagerWithDrainedWaitsForDispatchQueueAndCleanup(t *testing.T) {
	m := NewManager(1)
	t.Cleanup(func() { quiesceStop(t, m) })
	started, release := make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	var seen atomic.Int32
	paths := []string{filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")}
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("body"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.Register(quiescePluginFunc(func(_ context.Context, r Record) {
		if r.Model == "first" {
			close(started)
			<-release
		}
		seen.Add(1)
	}))
	m.Publish(context.Background(), Record{Model: "first", OutputContentPath: paths[0]})
	quiesceWait(t, started, "first plugin")
	m.Publish(context.Background(), Record{Model: "second", OutputContentPath: paths[1]})
	thirdDone := make(chan struct{})
	go func() { m.Publish(context.Background(), Record{Model: "third"}); close(thirdDone) }()
	quiesceMustWait(t, thirdDone, "full-queue publisher")
	drained := make(chan struct{})
	go func() {
		m.WithDrained(func() {
			if seen.Load() != 3 {
				t.Errorf("drained after %d records, want 3", seen.Load())
			}
			for _, p := range paths {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("content cleanup incomplete: %s (%v)", p, err)
				}
			}
		})
		close(drained)
	}()
	quiesceMustWait(t, drained, "drain")
	close(release)
	quiesceWait(t, drained, "all records drained")
	quiesceWait(t, thirdDone, "third publish")
	m.Publish(context.Background(), Record{Model: "after"})
	m.WithDrained(func() {
		if seen.Load() != 4 {
			t.Errorf("manager not reusable: seen=%d", seen.Load())
		}
	})
}

func TestManagerWithDrainedPausesOnlyDuringCallback(t *testing.T) {
	m := NewManager(4)
	t.Cleanup(func() { quiesceStop(t, m) })
	callback, release, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	observed, published := make(chan struct{}), make(chan struct{})
	m.SetPublishObserver(func(Record) { close(observed) })
	go func() { m.WithDrained(func() { close(callback); <-release }); close(drained) }()
	quiesceWait(t, callback, "close callback")
	go func() { m.Publish(context.Background(), Record{}); close(published) }()
	quiesceMustWait(t, observed, "new publication observer")
	quiesceMustWait(t, published, "new publication")
	close(release)
	quiesceWait(t, drained, "close callback completion")
	quiesceWait(t, published, "resumed publication")
	quiesceWait(t, observed, "resumed observer")
}

func TestManagerWithDrainedAllowsRecursivePublishWhileDraining(t *testing.T) {
	for _, kind := range []string{"observer", "plugin", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			m := NewManager(8)
			t.Cleanup(func() { quiesceStop(t, m) })
			entered, release, published, drained := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			quiesceUnblockOnCleanup(t, release)
			var seen atomic.Int32
			recurse := func(r Record) {
				if r.Model == "parent" {
					close(entered)
					<-release
					m.Publish(context.Background(), Record{Model: "child"})
				}
			}
			switch kind {
			case "observer":
				m.SetPublishObserver(recurse)
				m.Register(quiescePluginFunc(func(_ context.Context, _ Record) { seen.Add(1) }))
			case "plugin":
				m.Register(quiescePluginFunc(func(_ context.Context, r Record) { recurse(r); seen.Add(1) }))
			case "overflow":
				m.SetOverflowHandler(func(r Record) bool { recurse(r); seen.Add(1); return true })
				m.Stop()
			}
			go func() { m.Publish(context.Background(), Record{Model: "parent"}); close(published) }()
			quiesceWait(t, entered, kind+" callback")
			go func() {
				m.WithDrained(func() {
					if seen.Load() != 2 {
						t.Errorf("drained %s saw %d records", kind, seen.Load())
					}
				})
				close(drained)
			}()
			quiesceMustWait(t, drained, "drain during "+kind)
			close(release)
			quiesceWait(t, published, "recursive "+kind+" publish")
			quiesceWait(t, drained, "recursive "+kind+" drain")
		})
	}
}

// Overflow runs on the publisher, not the dispatcher. A stopped worker's Wait
// may already be satisfied while overflow still owns temp-file content.
func TestManagerWithDrainedWaitsForOverflowAndContentCleanup(t *testing.T) {
	m := NewManager(1)
	t.Cleanup(func() { quiesceStop(t, m) })
	path := filepath.Join(t.TempDir(), "deferred-body")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	entered, release, drained := make(chan struct{}), make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	m.SetOverflowHandler(func(r Record) bool {
		close(entered)
		<-release
		b, err := os.ReadFile(r.OutputContentPath)
		if err != nil || string(b) != "payload" {
			t.Errorf("overflow lost content: %q %v", b, err)
		}
		return true
	})
	m.Stop()
	go m.Publish(context.Background(), Record{OutputContentPath: path})
	quiesceWait(t, entered, "overflow handler")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !m.Wait(ctx) {
		t.Fatal("stopped dispatcher should finish even while publisher overflow is active")
	}
	go func() {
		m.WithDrained(func() {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("cleanup not finished at drain: %v", err)
			}
		})
		close(drained)
	}()
	quiesceMustWait(t, drained, "overflow drain")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("content removed while overflow owns it: %v", err)
	}
	close(release)
	quiesceWait(t, drained, "overflow completion and cleanup")
}

func TestManagerWithDrainedSerializesCallbacks(t *testing.T) {
	m := NewManager(4)
	t.Cleanup(func() { quiesceStop(t, m) })
	first, release, firstDone, second, secondDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	var active atomic.Int32
	go func() {
		m.WithDrained(func() { active.Add(1); close(first); <-release; active.Add(-1) })
		close(firstDone)
	}()
	quiesceWait(t, first, "first drain callback")
	go func() {
		m.WithDrained(func() {
			if active.Add(1) != 1 {
				t.Error("drain callbacks overlapped")
			}
			close(second)
			active.Add(-1)
		})
		close(secondDone)
	}()
	quiesceMustWait(t, second, "second drain callback")
	close(release)
	quiesceWait(t, firstDone, "first drain")
	quiesceWait(t, secondDone, "second drain")
}

func TestManagerWithDrainedRestoresPublicationAfterPanic(t *testing.T) {
	m := NewManager(4)
	t.Cleanup(func() { quiesceStop(t, m) })
	callback, release, recovered, published := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	var seen atomic.Int32
	m.Register(quiescePluginFunc(func(context.Context, Record) { seen.Add(1) }))
	go func() {
		defer func() {
			if recover() == nil {
				t.Error("callback panic did not propagate")
			}
			close(recovered)
		}()
		m.WithDrained(func() { close(callback); <-release; panic("fixture") })
	}()
	quiesceWait(t, callback, "panicking callback")
	go func() { m.Publish(context.Background(), Record{}); close(published) }()
	quiesceMustWait(t, published, "publication during callback")
	close(release)
	quiesceWait(t, recovered, "panic recovery")
	quiesceWait(t, published, "publication after panic")
	m.WithDrained(func() {
		if seen.Load() != 1 {
			t.Errorf("seen=%d want 1", seen.Load())
		}
	})
}

func TestManagerWithDrainedBeforeStartAndAfterStop(t *testing.T) {
	m := NewManager(1)
	m.WithDrained(func() {})
	if m.started.Load() {
		t.Error("draining idle manager needlessly started worker")
	}
	var overflow atomic.Int32
	m.SetOverflowHandler(func(Record) bool { overflow.Add(1); return true })
	m.Stop()
	m.WithDrained(func() {})
	m.Publish(context.Background(), Record{})
	m.WithDrained(func() {
		if overflow.Load() != 1 {
			t.Errorf("overflow=%d", overflow.Load())
		}
	})
	quiesceStop(t, m)
}

func TestManagerWithDrainedStopDuringCallback(t *testing.T) {
	m := NewManager(1)
	t.Cleanup(func() { quiesceStop(t, m) })
	m.Start(context.Background())
	var overflow atomic.Int32
	m.SetOverflowHandler(func(Record) bool { overflow.Add(1); return true })
	callback, release, drained, published := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	quiesceUnblockOnCleanup(t, release)
	go func() { m.WithDrained(func() { close(callback); <-release }); close(drained) }()
	quiesceWait(t, callback, "close callback")
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !m.Wait(ctx) {
		t.Fatal("stopped idle worker did not exit while barrier callback ran")
	}
	go func() { m.Publish(context.Background(), Record{}); close(published) }()
	quiesceMustWait(t, published, "stopped-manager publication while paused")
	close(release)
	quiesceWait(t, drained, "barrier after Stop")
	quiesceWait(t, published, "overflow publication after barrier")
	m.WithDrained(func() {
		if overflow.Load() != 1 {
			t.Errorf("overflow=%d, want 1", overflow.Load())
		}
	})
}

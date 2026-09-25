package jobsnapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func get(t *testing.T, store Store, kind, id string) Snapshot {
	t.Helper()
	snap, ok, err := store.Get(context.Background(), kind, id)
	if err != nil || !ok {
		t.Fatalf("Get(%s) ok=%v err=%v", id, ok, err)
	}
	return snap
}

func TestPublisherWritesFirstAndFinalAtOnceAndCoalescesProgress(t *testing.T) {
	store := NewMemoryStore()
	pub := NewPublisher(store, "image_generation_test", time.Minute, func() string { return "node-a" })
	pub.SetIntervals(time.Hour, time.Hour)
	t.Cleanup(pub.Close)

	pub.Publish(Snapshot{ID: "job-1", TenantID: "t1", Status: "queued", Version: 1})
	if got := get(t, store, "image_generation_test", "job-1"); got.Status != "queued" || got.OwnerNode != "node-a" {
		t.Fatalf("first snapshot must be written immediately: %+v", got)
	}
	for v := int64(2); v <= 5; v++ {
		pub.Publish(Snapshot{ID: "job-1", TenantID: "t1", Status: "running", Phase: "phase", Version: v})
	}
	if saves := store.Saves(); saves != 1 {
		t.Fatalf("progress inside the flush interval must be coalesced, saves=%d", saves)
	}
	pub.Publish(Snapshot{ID: "job-1", TenantID: "t1", Status: "succeeded", Terminal: true, Result: []byte(`{"ok":true}`), Version: 6})
	got := get(t, store, "image_generation_test", "job-1")
	if got.Status != "succeeded" || !got.Terminal || string(got.Result) != `{"ok":true}` {
		t.Fatalf("final snapshot must be written immediately: %+v", got)
	}
	// A progress write that arrives late must not overwrite the outcome.
	pub.Publish(Snapshot{ID: "job-1", Status: "running", Version: 3})
	if got := get(t, store, "image_generation_test", "job-1"); got.Status != "succeeded" {
		t.Fatalf("stale snapshot replaced the final one: %+v", got)
	}
	if _, ok, _ := store.Get(context.Background(), "model_test", "job-1"); ok {
		t.Fatal("a snapshot must only be readable under its own kind")
	}
}

func TestPublisherFlushesCoalescedProgress(t *testing.T) {
	store := NewMemoryStore()
	pub := NewPublisher(store, "k", time.Minute, func() string { return "node-a" })
	pub.SetIntervals(20*time.Millisecond, time.Hour)
	t.Cleanup(pub.Close)

	pub.Publish(Snapshot{ID: "job-2", Status: "queued", Version: 1})
	pub.Publish(Snapshot{ID: "job-2", Status: "running", Phase: "uploading", Version: 2})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok, _ := store.Get(context.Background(), "k", "job-2"); ok && snap.Phase == "uploading" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("coalesced progress was never flushed")
}

func TestPublisherHeartbeatAndLostOwner(t *testing.T) {
	store := NewMemoryStore()
	store.SetOwnerLostAfter(60 * time.Millisecond)
	pub := NewPublisher(store, "k", time.Minute, func() string { return "node-a" })
	pub.SetIntervals(time.Hour, 10*time.Millisecond)

	pub.Publish(Snapshot{ID: "job-3", Status: "running", Version: 1})
	time.Sleep(150 * time.Millisecond)
	if snap := get(t, store, "k", "job-3"); snap.OwnerLost {
		t.Fatal("a heartbeating owner must not read as lost")
	}
	pub.Close()
	time.Sleep(150 * time.Millisecond)
	if snap := get(t, store, "k", "job-3"); !snap.OwnerLost {
		t.Fatal("an unfinished job whose owner stopped heartbeating must read as lost")
	}
}

type flakyStore struct {
	*MemoryStore
	mu       sync.Mutex
	failures int
}

func (f *flakyStore) Save(ctx context.Context, snap Snapshot, ttl time.Duration) error {
	f.mu.Lock()
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return errors.New("database unavailable")
	}
	f.mu.Unlock()
	return f.MemoryStore.Save(ctx, snap, ttl)
}

func TestPublisherRetriesFailedFinalWrite(t *testing.T) {
	store := &flakyStore{MemoryStore: NewMemoryStore(), failures: 1}
	pub := NewPublisher(store, "k", time.Minute, func() string { return "node-a" })
	t.Cleanup(pub.Close)

	// Shorten the retry by publishing through a fresh flush once the first
	// attempt has failed.
	pub.Publish(Snapshot{ID: "job-4", Status: "failed", Terminal: true, Version: 1})
	if _, ok, _ := store.Get(context.Background(), "k", "job-4"); ok {
		t.Fatal("the first write was expected to fail")
	}
	pub.flush("job-4")
	if snap := get(t, store, "k", "job-4"); snap.Status != "failed" {
		t.Fatalf("retried final snapshot = %+v", snap)
	}
}

// slowStore blocks every save after the first until released, like a
// database in the middle of a failover.
type slowStore struct {
	*MemoryStore
	mu      sync.Mutex
	saves   int
	release chan struct{}
}

func (s *slowStore) Save(ctx context.Context, snap Snapshot, ttl time.Duration) error {
	s.mu.Lock()
	s.saves++
	blocked := s.saves > 1
	s.mu.Unlock()
	if blocked {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.MemoryStore.Save(ctx, snap, ttl)
}

func TestPublisherProgressNeverBlocksCaller(t *testing.T) {
	store := &slowStore{MemoryStore: NewMemoryStore(), release: make(chan struct{})}
	pub := NewPublisher(store, "k", time.Minute, func() string { return "node-a" })
	pub.SetIntervals(time.Millisecond, time.Hour)
	t.Cleanup(pub.Close)

	pub.Publish(Snapshot{ID: "job-5", Status: "running", Version: 1})
	time.Sleep(5 * time.Millisecond)
	started := time.Now()
	for v := int64(2); v <= 5; v++ {
		pub.Publish(Snapshot{ID: "job-5", Status: "running", Phase: "p", Version: v})
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("progress publishing blocked the caller for %s", elapsed)
	}
	time.Sleep(20 * time.Millisecond)
	store.mu.Lock()
	inFlight := store.saves
	store.mu.Unlock()
	if inFlight > 2 {
		t.Fatalf("background writes piled up behind a stuck database: %d saves issued", inFlight)
	}
	close(store.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok, _ := store.Get(context.Background(), "k", "job-5"); ok && snap.Version == 5 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the latest progress was never written once the store recovered")
}

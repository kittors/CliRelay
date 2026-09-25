package usage

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

type blockingPlugin struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingPlugin) HandleUsage(context.Context, Record) {
	p.once.Do(func() { close(p.started) })
	<-p.release
}

func TestManagerBoundsPendingQueue(t *testing.T) {
	manager := NewManager(1)
	plugin := &blockingPlugin{started: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)

	manager.Publish(context.Background(), Record{Model: "first"})
	select {
	case <-plugin.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start first record")
	}

	manager.Publish(context.Background(), Record{Model: "second"})
	thirdDone := make(chan struct{})
	go func() {
		manager.Publish(context.Background(), Record{Model: "third"})
		close(thirdDone)
	}()

	select {
	case <-thirdDone:
		t.Fatal("third publish must wait while the bounded queue is full")
	case <-time.After(50 * time.Millisecond):
	}

	close(plugin.release)
	select {
	case <-thirdDone:
	case <-time.After(time.Second):
		t.Fatal("third publish did not resume after queue capacity became available")
	}
	manager.Stop()
}

type observingPlugin struct {
	seen chan Record
}

func (p *observingPlugin) HandleUsage(_ context.Context, record Record) {
	p.seen <- record
}

func TestManagerCleansDeferredContentAfterDispatch(t *testing.T) {
	file, err := os.CreateTemp("", "usage-manager-test-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	path := file.Name()
	if _, err = file.WriteString("payload"); err != nil {
		t.Fatalf("write temp content: %v", err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("close temp content: %v", err)
	}

	manager := NewManager(1)
	plugin := &observingPlugin{seen: make(chan Record, 1)}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{OutputContentPath: path})

	select {
	case record := <-plugin.seen:
		if record.OutputContentPath != path {
			t.Fatalf("path = %q, want %q", record.OutputContentPath, path)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin did not receive record")
	}

	deadline := time.Now().Add(time.Second)
	for {
		_, err = os.Stat(path)
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deferred content was not cleaned, stat err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.Stop()
}

type contextCapturePlugin struct {
	seen chan context.Context
}

func (p *contextCapturePlugin) HandleUsage(ctx context.Context, _ Record) {
	p.seen <- ctx
}

func TestManagerDoesNotRetainRequestContext(t *testing.T) {
	type requestContextKey struct{}
	manager := NewManager(1)
	plugin := &contextCapturePlugin{seen: make(chan context.Context, 1)}
	manager.Register(plugin)
	requestCtx := context.WithValue(context.Background(), requestContextKey{}, "large-request-state")
	manager.Publish(requestCtx, Record{APIIdentifier: "POST /v1/responses"})

	select {
	case pluginCtx := <-plugin.seen:
		if got := pluginCtx.Value(requestContextKey{}); got != nil {
			t.Fatalf("async plugin retained request context value: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin did not receive record")
	}
	manager.Stop()
}

func TestManagerOverflowHandlerKeepsPublishNonBlocking(t *testing.T) {
	manager := NewManager(1)
	plugin := &blockingPlugin{started: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	overflowed := make(chan Record, 4)
	manager.SetOverflowHandler(func(record Record) bool {
		overflowed <- record
		return true
	})

	manager.Publish(context.Background(), Record{Model: "first"})
	select {
	case <-plugin.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start first record")
	}
	manager.Publish(context.Background(), Record{Model: "second"})

	thirdDone := make(chan struct{})
	go func() {
		manager.Publish(context.Background(), Record{Model: "third"})
		close(thirdDone)
	}()
	select {
	case <-thirdDone:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on a full queue although an overflow handler is installed")
	}
	select {
	case record := <-overflowed:
		if record.Model != "third" {
			t.Fatalf("overflowed model = %q, want third", record.Model)
		}
		if record.IdempotencyKey == "" {
			t.Fatal("overflowed record has no idempotency key")
		}
	default:
		t.Fatal("overflow handler did not receive the record the queue could not take")
	}

	close(plugin.release)
	manager.Stop()
}

func TestManagerOverflowHandlerReleasesWaitingPublishers(t *testing.T) {
	manager := NewManager(1)
	plugin := &blockingPlugin{started: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{Model: "first"})
	<-plugin.started
	manager.Publish(context.Background(), Record{Model: "second"})

	waiting := make(chan struct{})
	go func() {
		manager.Publish(context.Background(), Record{Model: "third"})
		close(waiting)
	}()
	select {
	case <-waiting:
		t.Fatal("publish must wait while no overflow handler is installed")
	case <-time.After(50 * time.Millisecond):
	}

	overflowed := make(chan Record, 1)
	manager.SetOverflowHandler(func(record Record) bool {
		overflowed <- record
		return true
	})
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("installing an overflow handler did not release the waiting publisher")
	}
	if record := <-overflowed; record.Model != "third" {
		t.Fatalf("overflowed model = %q, want third", record.Model)
	}
	close(plugin.release)
	manager.Stop()
}

func TestManagerAssignsIdempotencyKeyOnce(t *testing.T) {
	manager := NewManager(4)
	plugin := &observingPlugin{seen: make(chan Record, 2)}
	manager.Register(plugin)

	manager.Publish(context.Background(), Record{Model: "generated"})
	manager.Publish(context.Background(), Record{Model: "preset", IdempotencyKey: "caller-key"})

	first := <-plugin.seen
	second := <-plugin.seen
	if first.IdempotencyKey == "" {
		t.Fatal("Publish did not assign an idempotency key")
	}
	if second.IdempotencyKey != "caller-key" {
		t.Fatalf("preset key = %q, want caller-key", second.IdempotencyKey)
	}
	manager.Stop()
}

func TestManagerStoppedPublishGoesToOverflowHandler(t *testing.T) {
	manager := NewManager(4)
	manager.Start(context.Background())
	overflowed := make(chan Record, 1)
	manager.SetOverflowHandler(func(record Record) bool {
		overflowed <- record
		return true
	})
	manager.Stop()
	manager.Publish(context.Background(), Record{Model: "late"})
	select {
	case record := <-overflowed:
		if record.Model != "late" {
			t.Fatalf("overflowed model = %q, want late", record.Model)
		}
	default:
		t.Fatal("a record published after Stop was dropped instead of overflowed")
	}
}

func TestManagerWaitReturnsAfterQueueDrains(t *testing.T) {
	manager := NewManager(8)
	plugin := &blockingPlugin{started: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{Model: "first"})
	manager.Publish(context.Background(), Record{Model: "second"})
	<-plugin.started
	manager.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if manager.Wait(ctx) {
		cancel()
		t.Fatal("Wait returned before the blocked dispatcher drained its queue")
	}
	cancel()

	close(plugin.release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !manager.Wait(ctx) {
		t.Fatal("Wait did not observe the dispatcher exiting after the queue drained")
	}
	if pending := manager.Pending(); pending != 0 {
		t.Fatalf("pending = %d after drain, want 0", pending)
	}
	if !NewManager(1).Wait(context.Background()) {
		t.Fatal("Wait on a never-started manager must return immediately")
	}
}

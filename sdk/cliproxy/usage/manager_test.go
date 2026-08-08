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

func TestManagerRestartsAfterStop(t *testing.T) {
	manager := NewManager(2)
	plugin := &observingPlugin{seen: make(chan Record, 2)}
	manager.Register(plugin)

	manager.Publish(context.Background(), Record{Model: "before-stop"})
	select {
	case record := <-plugin.seen:
		if record.Model != "before-stop" {
			t.Fatalf("first record model = %q, want before-stop", record.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin did not receive first record")
	}

	manager.Stop()
	manager.Start(context.Background())
	manager.Publish(context.Background(), Record{Model: "after-restart"})

	select {
	case record := <-plugin.seen:
		if record.Model != "after-restart" {
			t.Fatalf("restarted record model = %q, want after-restart", record.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin did not receive record after manager restart")
	}
	manager.Stop()
}

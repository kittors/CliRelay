package usage

import (
	"context"
	"sync"
	"testing"
	"time"
)

type pluginFunc func(context.Context, Record)

func (f pluginFunc) HandleUsage(ctx context.Context, record Record) { f(ctx, record) }

func TestThePublishObserverHearsOfARecordBeforeAnyPlugin(t *testing.T) {
	m := NewManager(8)
	t.Cleanup(m.Stop)
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	delivered := make(chan struct{}, 4)
	m.Register(pluginFunc(func(_ context.Context, r Record) {
		note("plugin " + r.RequestID)
		delivered <- struct{}{}
	}))
	m.SetPublishObserver(func(r Record) { note("observer " + r.RequestID) })

	m.Publish(context.Background(), Record{RequestID: "r1"})
	waitDelivered(t, delivered)
	m.SetPublishObserver(func(Record) { panic("observer bug") })
	m.Publish(context.Background(), Record{RequestID: "r2"})
	waitDelivered(t, delivered)
	m.SetPublishObserver(nil)
	m.Publish(context.Background(), Record{RequestID: "r3"})
	waitDelivered(t, delivered)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"observer r1", "plugin r1", "plugin r2", "plugin r3"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func waitDelivered(t *testing.T, delivered <-chan struct{}) {
	t.Helper()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("record was not delivered")
	}
}

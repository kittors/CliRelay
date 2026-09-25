package cluster

import (
	"context"
	"testing"
)

func TestSingleNodeDefaults(t *testing.T) {
	c := newCoordinator(false, "solo")
	if c.Enabled() || !c.IsLeader() || c.ActiveNodeCount() != 1 {
		t.Fatalf("single node must be leader with count 1, got enabled=%v leader=%v count=%d", c.Enabled(), c.IsLeader(), c.ActiveNodeCount())
	}
	if err := c.Publish(context.Background(), TopicAuth, AuthEvent{ID: "x"}); err != nil {
		t.Fatalf("publish in single-node mode must be a no-op: %v", err)
	}
}

func TestMemoryHubDeliversToPeersOnly(t *testing.T) {
	hub := NewMemoryHub()
	a, b := hub.Join("a"), hub.Join("b")
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("first joiner leads: a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
	if a.ActiveNodeCount() != 2 || b.ActiveNodeCount() != 2 {
		t.Fatalf("active count = %d/%d, want 2", a.ActiveNodeCount(), b.ActiveNodeCount())
	}
	var gotA, gotB []AuthEvent
	a.Subscribe(TopicAuth, func(ev Event) {
		var p AuthEvent
		_ = ev.Decode(&p)
		gotA = append(gotA, p)
	})
	b.Subscribe(TopicAuth, func(ev Event) {
		var p AuthEvent
		_ = ev.Decode(&p)
		gotB = append(gotB, p)
	})
	if err := a.Publish(context.Background(), TopicAuth, AuthEvent{ID: "t/1.json", Version: 3}); err != nil {
		t.Fatal(err)
	}
	if len(gotA) != 0 || len(gotB) != 1 || gotB[0].Version != 3 {
		t.Fatalf("publisher must not see its own event; peer must: a=%v b=%v", gotA, gotB)
	}
}

func TestMemoryHubResyncAfterReconnect(t *testing.T) {
	hub := NewMemoryHub()
	a, b := hub.Join("a"), hub.Join("b")
	var events []Event
	b.Subscribe(TopicConfig, func(ev Event) { events = append(events, ev) })
	hub.Disconnect("b")
	if b.ActiveNodeCount() != 1 {
		t.Fatalf("disconnected node must drop out of the active count")
	}
	_ = a.Publish(context.Background(), TopicConfig, ConfigEvent{Domain: "api_keys"})
	if len(events) != 0 {
		t.Fatalf("event published while disconnected must be lost, got %v", events)
	}
	hub.Reconnect("b")
	if len(events) != 1 || !events[0].Resync {
		t.Fatalf("reconnect must deliver one resync event, got %+v", events)
	}
}

func TestMemoryHubLeadershipCallbacks(t *testing.T) {
	hub := NewMemoryHub()
	a, b := hub.Join("a"), hub.Join("b")
	var changes []bool
	b.OnLeadershipChange(func(v bool) { changes = append(changes, v) })
	hub.Leave("a")
	if a.IsLeader() {
		t.Fatalf("departed node must not stay leader")
	}
	hub.SetLeader("b")
	if !b.IsLeader() || len(changes) != 1 || !changes[0] {
		t.Fatalf("b must gain leadership exactly once, changes=%v", changes)
	}
}

func TestLockKeyStable(t *testing.T) {
	if LockKey(LockMigrate) != LockKey("migrate") || LockKey(LockMigrate) == LockKey(LockLeader) {
		t.Fatalf("lock keys must be stable and distinct")
	}
}

func TestPayloadLimit(t *testing.T) {
	hub := NewMemoryHub()
	a := hub.Join("a")
	big := make([]byte, maxPayloadBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := a.Publish(context.Background(), TopicAuth, string(big)); err == nil {
		t.Fatalf("oversized payload must be rejected")
	}
}

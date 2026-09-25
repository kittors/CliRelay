package clusterstate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

func TestJanitorSweepsOnLeaderOnly(t *testing.T) {
	hub := cluster.NewMemoryHub()
	nodeA, nodeB := hub.Join("node-a"), hub.Join("node-b")
	var sweepsA, sweepsB atomic.Int32
	stopA := StartJanitor(5*time.Millisecond, nodeA.IsLeader, func(context.Context) error { sweepsA.Add(1); return nil })
	stopB := StartJanitor(5*time.Millisecond, nodeB.IsLeader, func(context.Context) error { sweepsB.Add(1); return nil })
	defer stopA()
	defer stopB()

	waitFor(t, func() bool { return sweepsA.Load() >= 2 })
	if sweepsB.Load() != 0 {
		t.Fatalf("a follower swept %d times", sweepsB.Load())
	}

	hub.SetLeader("node-b")
	waitFor(t, func() bool { return sweepsB.Load() >= 2 })
	before := sweepsA.Load()
	time.Sleep(30 * time.Millisecond)
	// One sweep may already have been past its leader check when leadership moved.
	if after := sweepsA.Load(); after > before+1 {
		t.Fatalf("the former leader kept sweeping: %d -> %d", before, after)
	}

	stopB()
	stopped := sweepsB.Load()
	time.Sleep(30 * time.Millisecond)
	if sweepsB.Load() != stopped {
		t.Fatal("a stopped janitor must not sweep")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

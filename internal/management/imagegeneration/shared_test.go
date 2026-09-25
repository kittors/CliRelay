package imagegeneration

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// sharedNodes builds the same task service on two cluster nodes over one
// snapshot store. Node A runs tasks through execute; node B only answers
// polls.
func sharedNodes(t *testing.T, execute ExecuteFunc) (store *jobsnapshot.MemoryStore, nodeA, nodeB *Service) {
	t.Helper()
	hub := cluster.NewMemoryHub()
	coordA, coordB := hub.Join("node-a"), hub.Join("node-b")
	store = jobsnapshot.NewMemoryStore()
	nodeA = NewService(execute, "test")
	nodeA.ShareSnapshots(store, "image_generation_test", coordA.NodeID)
	nodeA.shared.SetIntervals(time.Millisecond, 10*time.Millisecond)
	nodeB = NewService(func(context.Context, string, []byte, string) ([]byte, error) {
		t.Error("node B must not run node A's task")
		return nil, nil
	}, "test")
	nodeB.ShareSnapshots(store, "image_generation_test", coordB.NodeID)
	t.Cleanup(nodeA.shared.Close)
	t.Cleanup(nodeB.shared.Close)
	return store, nodeA, nodeB
}

func waitShared(t *testing.T, svc *Service, tenantID, taskID string, done func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last Snapshot
	for time.Now().Before(deadline) {
		snap, ok := svc.Get(tenantID, taskID)
		if ok {
			last = snap
			if done(snap) {
				return snap
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s never reached the expected state; last=%+v", taskID, last)
	return Snapshot{}
}

// Node A runs the task; node B sees it from the first poll, follows its
// phases and receives the result.
func TestSharedTaskProgressAndResultVisibleOnAnotherNode(t *testing.T) {
	release := make(chan struct{})
	_, nodeA, nodeB := sharedNodes(t, func(ctx context.Context, _ string, _ []byte, _ string) ([]byte, error) {
		if hook, ok := ctx.Value(util.ContextKeyImageGenerationPhaseHook).(func(string)); ok {
			hook("generating")
		}
		<-release
		return []byte(`{"data":[{"b64_json":"aGVsbG8="}]}`), nil
	})

	started := nodeA.Start("tenant-a", []byte(`{"prompt":"a fox"}`), "images/generations")
	if snap, ok := nodeB.Get("tenant-a", started.ID); !ok || snap.ID != started.ID {
		t.Fatalf("node B must see the task as soon as its id is out: %+v ok=%v", snap, ok)
	}
	waitShared(t, nodeB, "tenant-a", started.ID, func(s Snapshot) bool { return s.Phase == "generating" })
	if _, ok := nodeB.Get("tenant-b", started.ID); ok {
		t.Fatal("another tenant must not see the task")
	}

	close(release)
	final := waitShared(t, nodeB, "tenant-a", started.ID, func(s Snapshot) bool { return s.Status == "succeeded" })
	data, _ := final.Result.(map[string]any)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("node B result = %#v", final.Result)
	}
}

func TestSharedTaskReportsLostNode(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	store, nodeA, nodeB := sharedNodes(t, func(context.Context, string, []byte, string) ([]byte, error) {
		<-block
		return nil, context.Canceled
	})
	store.SetOwnerLostAfter(50 * time.Millisecond)

	started := nodeA.Start("tenant-a", []byte(`{}`), "images/generations")
	time.Sleep(120 * time.Millisecond)
	if snap, _ := nodeB.Get("tenant-a", started.ID); snap.Status == "failed" {
		t.Fatalf("a heartbeating node must not read as lost: %+v", snap)
	}
	// Node A dies: heartbeats stop, the task can never finish.
	nodeA.shared.Close()
	lost := waitShared(t, nodeB, "tenant-a", started.ID, func(s Snapshot) bool { return s.Status == "failed" })
	body, _ := lost.Error["body"].(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	if errBody["type"] != "node_unavailable" {
		t.Fatalf("lost task error = %#v", lost.Error)
	}
}

func TestSharedTaskOversizedResultStaysOnOwner(t *testing.T) {
	previous := maxSharedResultBytes
	maxSharedResultBytes = 16
	t.Cleanup(func() { maxSharedResultBytes = previous })
	_, nodeA, nodeB := sharedNodes(t, func(context.Context, string, []byte, string) ([]byte, error) {
		return []byte(`{"data":[{"b64_json":"a-result-well-over-sixteen-bytes"}]}`), nil
	})

	started := nodeA.Start("tenant-a", []byte(`{}`), "images/generations")
	owner := waitShared(t, nodeA, "tenant-a", started.ID, func(s Snapshot) bool { return s.Status == "succeeded" })
	if owner.Result == nil {
		t.Fatal("the node that ran the task keeps the full result")
	}
	other := waitShared(t, nodeB, "tenant-a", started.ID, func(s Snapshot) bool { return s.Status == "failed" })
	body, _ := other.Error["body"].(map[string]any)
	errBody, _ := body["error"].(map[string]any)
	if errBody["type"] != "result_too_large" {
		t.Fatalf("oversized result on another node = %#v", other.Error)
	}
}

// Without a shared store the service answers from memory only, as before.
func TestUnsharedTaskIsLocalOnly(t *testing.T) {
	nodeA := NewService(func(context.Context, string, []byte, string) ([]byte, error) {
		return []byte(`{}`), nil
	}, "test")
	nodeB := NewService(nil, "test")
	started := nodeA.Start("tenant-a", []byte(`{}`), "images/generations")
	if _, ok := nodeB.Get("tenant-a", started.ID); ok {
		t.Fatal("an unshared task must not be visible elsewhere")
	}
}

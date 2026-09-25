package aiaccountstatus

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// sharedJobNodes gives node A a refresh service that probes through probe,
// and node B an instance that only answers polls, both over one store.
func sharedJobNodes(t *testing.T, probe ProbeFunc) (store *jobsnapshot.MemoryStore, nodeA, nodeB *Service) {
	t.Helper()
	hub := cluster.NewMemoryHub()
	coordA, coordB := hub.Join("node-a"), hub.Join("node-b")
	store = jobsnapshot.NewMemoryStore()

	auth := &coreauth.Auth{ID: "id-a", Provider: "codex", FileName: "a.json", Metadata: map[string]any{"account_id": "acct-a"}}
	manager := newTestManager(t, "tenant-1", auth)
	nodeA = New(&config.Config{}, manager, func(string) *managementapitools.Service {
		return managementapitools.NewForTenant("tenant-1", &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	nodeA.SetProbeFunc(probe)
	nodeA.ShareJobSnapshots(store, coordA.NodeID)
	nodeA.sharedJobs.SetIntervals(time.Millisecond, 10*time.Millisecond)

	nodeB = New(&config.Config{}, coreauth.NewManager(nil, nil, nil), nil, nil)
	nodeB.ShareJobSnapshots(store, coordB.NodeID)
	t.Cleanup(nodeA.sharedJobs.Close)
	t.Cleanup(nodeB.sharedJobs.Close)
	return store, nodeA, nodeB
}

func waitJobOn(t *testing.T, svc *Service, jobID string, done func(JobSnapshot) bool) JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last JobSnapshot
	for time.Now().Before(deadline) {
		if snap, ok := svc.GetJob("tenant-1", jobID); ok {
			last = snap
			if done(snap) {
				return snap
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never reached the expected state; last=%+v", jobID, last)
	return JobSnapshot{}
}

// Node A runs the refresh; node B follows its progress and sees the outcome.
func TestSharedRefreshJobVisibleOnAnotherNode(t *testing.T) {
	release := make(chan struct{})
	_, nodeA, nodeB := sharedJobNodes(t, func(ctx context.Context, _ *managementapitools.Service, _ *config.Config, _ *coreauth.Auth) (ProbeResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ProbeResult{}, context.DeadlineExceeded
	})

	accepted := nodeA.StartRefresh("tenant-1", RefreshRequest{Force: true})
	if accepted.Accepted != 1 {
		t.Fatalf("accepted = %+v", accepted)
	}
	running := waitJobOn(t, nodeB, accepted.JobID, func(s JobSnapshot) bool {
		return len(s.Results) == 1 && s.Results[0].State == RefreshRunning
	})
	if running.State != "running" || running.Completed != 0 {
		t.Fatalf("progress on node B = %+v", running)
	}
	if _, ok := nodeB.GetJob("tenant-2", accepted.JobID); ok {
		t.Fatal("another tenant must not see the job")
	}

	close(release)
	final := waitJobOn(t, nodeB, accepted.JobID, func(s JobSnapshot) bool { return s.State == "completed" })
	if final.Total != 1 || final.Failed != 1 || final.Results[0].ErrorCode != "probe_failed" {
		t.Fatalf("outcome on node B = %+v", final)
	}
}

func TestSharedRefreshJobReportsLostNode(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	store, nodeA, nodeB := sharedJobNodes(t, func(ctx context.Context, _ *managementapitools.Service, _ *config.Config, _ *coreauth.Auth) (ProbeResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return ProbeResult{}, nil
	})
	store.SetOwnerLostAfter(50 * time.Millisecond)

	accepted := nodeA.StartRefresh("tenant-1", RefreshRequest{Force: true})
	waitJobOn(t, nodeB, accepted.JobID, func(s JobSnapshot) bool { return s.State == "running" })
	nodeA.sharedJobs.Close()

	lost := waitJobOn(t, nodeB, accepted.JobID, func(s JobSnapshot) bool { return s.State == "completed" })
	if lost.Failed != 1 || lost.Results[0].ErrorCode != lostNodeCode {
		t.Fatalf("job of a lost node = %+v", lost)
	}
}

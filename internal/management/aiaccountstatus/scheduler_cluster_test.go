package aiaccountstatus

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSchedulerProbesOnlyOnLeader(t *testing.T) {
	hub := cluster.NewMemoryHub()
	leader, follower := hub.Join("node-a"), hub.Join("node-b")
	t.Cleanup(func() { cluster.SetDefault(nil) })
	rounds := 0
	s := NewScheduler(func() *Service { rounds++; return nil }, func() *coreauth.Manager { return nil }, time.Minute, 0)

	cluster.SetDefault(follower)
	s.runRound()
	if rounds != 0 {
		t.Fatalf("a follower ran %d probe rounds", rounds)
	}
	cluster.SetDefault(leader)
	s.runRound()
	if rounds != 1 {
		t.Fatalf("the leader ran %d probe rounds, want 1", rounds)
	}
}

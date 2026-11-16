package cluster

import (
	"context"
	"testing"
)

func TestSingleNodeReportsItsVersionAndProcessStart(t *testing.T) {
	c, err := Start(context.Background(), Options{NodeID: "solo", Version: " dev-1234567 "})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetDefault(nil) })
	nodes, err := c.Nodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	self := nodes[0]
	if self.NodeID != "solo" || self.Version != "dev-1234567" || !self.StartedAt.Equal(processStartedAt) ||
		!self.Leader || !self.Active || !self.Self || len(self.Snapshot) != 0 {
		t.Fatalf("self = %+v", self)
	}
}

// Self is what a cluster node reports while it cannot read the membership
// table: its own leadership as it knows it.
func TestSelfOfAClusterFollower(t *testing.T) {
	hub := NewMemoryHub()
	hub.Join("leader")
	follower := hub.Join("follower")
	self := follower.Self()
	if self.NodeID != "follower" || self.Leader || !self.Active || !self.Self {
		t.Fatalf("self = %+v", self)
	}
	var nilCoordinator *Coordinator
	if got := nilCoordinator.Self(); got.NodeID == "" || !got.Leader {
		t.Fatalf("a nil coordinator is a single node: %+v", got)
	}
}

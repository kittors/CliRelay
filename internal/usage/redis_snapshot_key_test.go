package usage

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

func TestRedisUsageSnapshotKeyIsPerNodeInCluster(t *testing.T) {
	cluster.SetDefault(nil)
	t.Cleanup(func() { cluster.SetDefault(nil) })
	if got := redisUsageSnapshotKey(); got != "cliproxy:usage_stats_snapshot" {
		t.Fatalf("a single node must keep the historical key, got %q", got)
	}

	hub := cluster.NewMemoryHub()
	cluster.SetDefault(hub.Join("node-a"))
	a := redisUsageSnapshotKey()
	cluster.SetDefault(hub.Join("node-b"))
	b := redisUsageSnapshotKey()
	if a != "cliproxy:usage_stats_snapshot:node-a" || b != "cliproxy:usage_stats_snapshot:node-b" {
		t.Fatalf("cluster nodes must not share a snapshot key: %q %q", a, b)
	}
}

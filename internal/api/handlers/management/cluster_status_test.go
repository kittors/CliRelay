package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/nodemetrics"
)

type clusterStatusBody struct {
	Enabled     bool                 `json:"enabled"`
	NodeID      string               `json:"node_id"`
	IsLeader    bool                 `json:"is_leader"`
	ActiveNodes int                  `json:"active_nodes"`
	Nodes       []cluster.NodeStatus `json:"nodes"`
	Hosts       []cluster.HostStatus `json:"hosts"`
}

// sampleNodeMetrics makes sure the process-wide collector has a snapshot.
func sampleNodeMetrics(t *testing.T) nodemetrics.Snapshot {
	t.Helper()
	collector := nodemetrics.Default()
	collector.Start()
	t.Cleanup(collector.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap, ok := collector.Latest(); ok {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatal("the collector took no snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func getClusterStatus(t *testing.T) clusterStatusBody {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cluster", nil)
	(&Handler{}).GetClusterStatus(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body clusterStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body
}

func TestGetClusterStatusSingleNode(t *testing.T) {
	cluster.SetDefault(nil)
	body := getClusterStatus(t)
	if body.Enabled || !body.IsLeader || body.ActiveNodes != 1 || len(body.Nodes) != 1 || !body.Nodes[0].Self || body.NodeID == "" {
		t.Fatalf("single node status = %+v", body)
	}
	// No PostgreSQL runtime database here, so no cluster_hosts to read.
	if body.Hosts != nil {
		t.Fatalf("hosts = %+v, want none without PostgreSQL", body.Hosts)
	}
}

// The panel shows the node view only where GET /config declares it.
func TestNodeMetricsCapabilityIsDeclaredInConfig(t *testing.T) {
	raw, err := json.Marshal(newConfigResponse(nil, nil).Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"node-metrics":true`) {
		t.Fatalf("capabilities JSON = %s, want node-metrics declared", raw)
	}
}

// A single node never writes the membership table, so its own row carries
// the snapshot taken in the process.
func TestGetClusterStatusSingleNodeCarriesItsSnapshot(t *testing.T) {
	cluster.SetDefault(nil)
	snap := sampleNodeMetrics(t)
	body := getClusterStatus(t)
	self := body.Nodes[0]
	if len(self.Snapshot) == 0 || self.SnapshotAt == nil || !self.SnapshotAt.Equal(snap.CollectedAt) {
		t.Fatalf("self row = %+v, want the latest snapshot taken at %s", self, snap.CollectedAt)
	}
	var decoded nodemetrics.Snapshot
	if err := json.Unmarshal(self.Snapshot, &decoded); err != nil || decoded.Schema != nodemetrics.SchemaVersion {
		t.Fatalf("snapshot %s does not decode: %v", self.Snapshot, err)
	}
	if self.StartedAt.After(time.Now()) || time.Since(self.StartedAt) < 0 {
		t.Fatalf("a single node reports its process start, got %s", self.StartedAt)
	}
}

func TestGetClusterStatusCluster(t *testing.T) {
	hub := cluster.NewMemoryHub()
	hub.Join("node-a")
	follower := hub.Join("node-b")
	cluster.SetDefault(follower)
	t.Cleanup(func() { cluster.SetDefault(nil) })
	body := getClusterStatus(t)
	if !body.Enabled || body.NodeID != "node-b" || body.IsLeader || body.ActiveNodes != 2 || len(body.Nodes) != 2 {
		t.Fatalf("cluster status = %+v", body)
	}
	for _, node := range body.Nodes {
		if node.Self != (node.NodeID == "node-b") || node.Leader != (node.NodeID == "node-a") {
			t.Fatalf("node view = %+v", node)
		}
	}
}

func TestGetClusterStatusClusterAttachesTheLocalSnapshotToSelf(t *testing.T) {
	hub := cluster.NewMemoryHub()
	hub.Join("node-a")
	follower := hub.Join("node-b")
	cluster.SetDefault(follower)
	t.Cleanup(func() { cluster.SetDefault(nil) })
	sampleNodeMetrics(t)
	body := getClusterStatus(t)
	for _, node := range body.Nodes {
		// The hub stores no snapshots: only this node's live one shows.
		if hasSnapshot := len(node.Snapshot) > 0; hasSnapshot != node.Self {
			t.Fatalf("node %s: snapshot present=%v, self=%v", node.NodeID, hasSnapshot, node.Self)
		}
	}
}

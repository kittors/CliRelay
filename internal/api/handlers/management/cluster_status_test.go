package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

type clusterStatusBody struct {
	Enabled     bool                 `json:"enabled"`
	NodeID      string               `json:"node_id"`
	IsLeader    bool                 `json:"is_leader"`
	ActiveNodes int                  `json:"active_nodes"`
	Nodes       []cluster.NodeStatus `json:"nodes"`
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

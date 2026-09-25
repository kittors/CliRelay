package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// GetClusterStatus reports this node's view of the cluster: whether cluster
// mode is on, which node answered, who leads, and the membership table. A
// single node reports itself as the only, leading member.
//
// The membership list is read from the database; if that fails the local
// fields are still returned, since they are what an operator needs most
// when the database is the problem.
func (h *Handler) GetClusterStatus(c *gin.Context) {
	coordinator := cluster.Default()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	nodes, err := coordinator.Nodes(ctx)
	if nodes == nil {
		nodes = []cluster.NodeStatus{}
	}
	body := gin.H{
		"enabled":      coordinator.Enabled(),
		"node_id":      coordinator.NodeID(),
		"is_leader":    coordinator.IsLeader(),
		"active_nodes": coordinator.ActiveNodeCount(),
		"nodes":        nodes,
	}
	if err != nil {
		body["nodes_error"] = err.Error()
	}
	c.JSON(http.StatusOK, body)
}

package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/nodemetrics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetClusterStatus reports this node's view of the cluster: whether cluster
// mode is on, which node answered, who leads, and the membership table with
// each node's resource snapshot (package nodemetrics). A single node reports
// itself as the only, leading member. The panel learns that the snapshots are
// there from the node-metrics capability of GET /config, like every other
// feature it must not offer to older backends.
//
// The membership list is read from the database; if that fails, this node
// still reports itself from what the process knows, next to the error, since
// that is what an operator needs most when the database is the problem.
//
// This node's own row carries the snapshot taken in this process, which is
// fresher than the stored copy and also exists on a single node, which never
// writes the membership table. The leader's snapshot also carries the state
// of the PostgreSQL server (nodemetrics.Database), and "hosts" lists the
// machines without a node that report through cmd/clirelay-hostagent, such
// as the database host.
func (h *Handler) GetClusterStatus(c *gin.Context) {
	coordinator := cluster.Default()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	nodes, err := coordinator.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		nodes = []cluster.NodeStatus{coordinator.Self()}
	}
	if raw, collectedAt := nodemetrics.Default().LatestJSON(); raw != nil {
		for i := range nodes {
			if nodes[i].Self {
				at := collectedAt
				nodes[i].Snapshot, nodes[i].SnapshotAt = raw, &at
			}
		}
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
	// Hosts without a node (cmd/clirelay-hostagent), on PostgreSQL only: the
	// table does not exist on SQLite. Left out when none reports.
	if usage.RuntimeDriver() == "postgres" {
		if db := usage.RuntimeDB(); db != nil {
			hosts, hostsErr := cluster.ListHosts(ctx, db)
			switch {
			case hostsErr != nil:
				body["hosts_error"] = hostsErr.Error()
			case len(hosts) > 0:
				body["hosts"] = hosts
			}
		}
	}
	c.JSON(http.StatusOK, body)
}

package api

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// clusterNodeHeader names the node that served a response, so an operator
// can tell which instance behind the load balancer answered.
const clusterNodeHeader = "X-CliRelay-Node"

// clusterNodeHeaderMiddleware sets clusterNodeHeader in cluster mode only;
// single-node responses are unchanged. It runs first, so rejected and
// failed requests carry the header too.
func clusterNodeHeaderMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if coordinator := cluster.Default(); coordinator.Enabled() {
			c.Header(clusterNodeHeader, coordinator.NodeID())
		}
		c.Next()
	}
}

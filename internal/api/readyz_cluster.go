package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// readyzRuntimeDB returns the shared runtime pool; tests replace it.
var readyzRuntimeDB = usage.RuntimeDB

// handleClusterReadyz is readiness in cluster mode. The database is shared
// by every node, so the probe pings the runtime pool this node serves from
// instead of dialing a fresh connection on every call. Redis is per-node
// state here: losing it degrades this node but must not pull it out of the
// load balancer, which would take every node sharing that Redis out at once.
func (s *Server) handleClusterReadyz(ctx context.Context, c *gin.Context) {
	db := readyzRuntimeDB()
	if db == nil || db.PingContext(ctx) != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "postgres"})
		return
	}
	var degraded []string
	if s.cfg.Redis.Enable {
		addr := strings.TrimSpace(s.cfg.Redis.Addr)
		switch {
		case addr == "":
			degraded = append(degraded, "redis_addr_missing")
		case pingRedis(ctx, addr, s.cfg.Redis.Password, s.cfg.Redis.DB) != nil:
			degraded = append(degraded, "redis")
		}
	}
	if len(degraded) > 0 {
		c.JSON(http.StatusOK, gin.H{"status": "degraded", "degraded": degraded})
		return
	}
	c.Status(http.StatusNoContent)
}

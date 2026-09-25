package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/egresshealth"
	sqlproxypool "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/proxypool"
	log "github.com/sirupsen/logrus"
)

// handleReadyzEgress reports whether this node can reach the proxies its
// upstream traffic leaves through. The arbiter's DNS watcher probes it next to
// /readyz: a node can be ready and still fail every proxied request once its
// network loses the route to the proxy provider.
//
// 204 means no proxy endpoint is unreachable right now, which includes having
// none and the first check still running at startup. The 503 body carries
// counts only: the endpoint is public, and proxy hosts are operator
// information.
func (s *Server) handleReadyzEgress(c *gin.Context) {
	status := s.egress.Status()
	if status.Unreachable == 0 {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"status":      "degraded",
		"unreachable": status.Unreachable,
		"total":       status.Total,
	})
}

// newEgressProber builds the prober behind /readyz/egress. Server.Start starts
// it and Server.Stop stops it, in single-node and cluster mode alike.
func (s *Server) newEgressProber() *egresshealth.Prober {
	sources := &egressSources{server: s}
	return egresshealth.New(egresshealth.Options{Targets: sources.targets})
}

// egressSources lists the proxies this node's upstream traffic may leave
// through: the global proxy-url and every enabled proxy-pool entry, of every
// tenant. Per-credential proxy URLs are left out: enumerating them means
// cloning every credential each round, and one credential's private proxy
// says little about the node.
//
// Only the prober's goroutine calls targets.
type egressSources struct {
	server *Server
	// stored is the last successful read of the stored proxy pools, kept so a
	// database hiccup does not drop endpoints from the check.
	stored []string
}

func (e *egressSources) targets(ctx context.Context) egresshealth.Targets {
	var targets egresshealth.Targets
	// The live config holds the global proxy-url and the system tenant's pool;
	// read them under the lock management writes take.
	e.server.liveConfig(func(cfg *config.Config) {
		targets.IPv4Only = cfg.PreferIPv4
		targets.ProxyURLs = append(targets.ProxyURLs, cfg.ProxyURL)
		for _, entry := range cfg.ProxyPool {
			if entry.Enabled {
				targets.ProxyURLs = append(targets.ProxyURLs, entry.URL)
			}
		}
	})
	// Other tenants' pools only live in the database.
	urls, err := sqlproxypool.ListEnabledURLs(ctx, readyzRuntimeDB())
	switch {
	case err == nil:
		e.stored = urls
	case ctx.Err() == nil:
		log.WithError(err).Debug("egress-health: cannot read the stored proxy pools; checking the last known list")
	}
	targets.ProxyURLs = append(targets.ProxyURLs, e.stored...)
	return targets
}

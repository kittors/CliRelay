package api

import (
	"context"
	"net"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/nodemetrics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// The node's resource snapshot (package nodemetrics): this server feeds the
// process-wide collector and runs it while it serves. The cluster heartbeat
// stores the latest snapshot in the membership table, and
// GET /v0/management/cluster shows it for every node.

// startNodeMetrics points the collector at this server and starts sampling.
func (s *Server) startNodeMetrics() {
	collector := nodemetrics.Default()
	collector.SetSources(nodemetrics.Sources{
		Version:   buildinfo.Version,
		InFlight:  s.inFlightRequests.Load,
		PingDB:    pingRuntimeDB,
		PingRedis: s.pingNodeRedis,
		Egress:    s.egressSnapshot,
	})
	collector.Start()
}

// stopNodeMetrics ends sampling. Stop calls it first: the cluster row stops
// being refreshed as soon as shutdown begins, and the sources it reads (the
// cluster runtime above all) are torn down after the drain.
func (s *Server) stopNodeMetrics() {
	nodemetrics.Default().Stop()
}

// listenCounted opens the server's listening socket, counting the bytes of
// every connection for the snapshot. fallback is the address net/http's
// ListenAndServe uses when none is set.
func (s *Server) listenCounted(fallback string) (net.Listener, error) {
	addr := s.server.Addr
	if addr == "" {
		addr = fallback
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return nodemetrics.Default().WrapListener(ln), nil
}

func pingRuntimeDB(ctx context.Context) error {
	db := usage.RuntimeDB()
	if db == nil {
		return nodemetrics.ErrNotConfigured
	}
	return db.PingContext(ctx)
}

// pingNodeRedis times the Redis this node depends on: the shared cluster
// Redis when there is one, the usage persistence Redis otherwise.
func (s *Server) pingNodeRedis(ctx context.Context) (string, error) {
	if configured, err := s.clusterRuntime.PingRedis(ctx); configured {
		return "cluster", err
	}
	if configured, err := usage.PingRedis(ctx); configured {
		return "usage", err
	}
	return "", nodemetrics.ErrNotConfigured
}

// egressSnapshot sums up the egress breaker and the connect check behind
// /readyz/egress. It counts proxies and never names them: the snapshot is
// stored and shown, and proxy labels carry hosts.
func (s *Server) egressSnapshot() *nodemetrics.Egress {
	breaker := egressbreaker.Default()
	out := &nodemetrics.Egress{BreakerEnabled: breaker.Enabled()}
	for _, status := range breaker.Open() {
		out.BreakerOpen++
		switch status.Fault.Code() {
		case "auth_rejected":
			out.AuthRejected++
		case "unreachable":
			out.Unreachable++
		}
		if since := status.Since.UTC(); !status.Since.IsZero() && (out.OpenSince == nil || since.Before(*out.OpenSince)) {
			out.OpenSince = &since
		}
	}
	if check := s.egress.Status(); check.Checked {
		out.Check = &nodemetrics.EgressCheck{Total: check.Total, Unreachable: check.Unreachable}
	}
	return out
}

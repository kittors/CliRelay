package api

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/nodemetrics"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/egressbreaker"
)

// The node's resource snapshot (package nodemetrics): this server feeds the
// process-wide collector and runs it while it serves. The cluster heartbeat
// stores the latest snapshot in the membership table, and
// GET /v0/management/cluster shows it for every node.

// nodeDiskPathsEnv lists extra directories, comma-separated, whose
// filesystems the node snapshot reports besides the ones the process writes
// to: a backup directory on its own volume, for instance.
const nodeDiskPathsEnv = "CLIRELAY_NODE_DISK_PATHS"

// diskPathsRefresh is how long the resolved directory list is reused.
// Resolving the log directory creates and removes a probe file, which the
// 5-second snapshot loop should not do every time.
const diskPathsRefresh = time.Minute

// startNodeMetrics points the collector at this server and starts sampling.
// The database probe samples the PostgreSQL server while this node leads.
func (s *Server) startNodeMetrics() {
	probe := nodemetrics.DefaultDatabaseProbe()
	probe.SetSources(runtimePostgresDB, func() bool { return cluster.Default().IsLeader() })
	probe.Start()

	collector := nodemetrics.Default()
	collector.SetSources(nodemetrics.Sources{
		Version:     buildinfo.Version,
		InFlight:    s.inFlightRequests.Load,
		PingDB:      pingRuntimeDB,
		PingRedis:   s.pingNodeRedis,
		Egress:      s.egressSnapshot,
		SharedRedis: s.sharedRedisSnapshot,
		DiskPaths:   s.cachedDiskPaths(),
		Database:    probe.Latest,
	})
	collector.Start()
}

// stopNodeMetrics ends sampling. Stop calls it first: the cluster row stops
// being refreshed as soon as shutdown begins, and the sources it reads (the
// cluster runtime above all) are torn down after the drain.
func (s *Server) stopNodeMetrics() {
	nodemetrics.Default().Stop()
	nodemetrics.DefaultDatabaseProbe().Stop()
}

// runtimePostgresDB is the runtime pool when it is PostgreSQL; the database
// probe has nothing to read on SQLite.
func runtimePostgresDB() *sql.DB {
	if usage.RuntimeDriver() != "postgres" {
		return nil
	}
	return usage.RuntimeDB()
}

// cachedDiskPaths returns the snapshot's directory source, which resolves
// the list at most once a minute.
func (s *Server) cachedDiskPaths() func() []nodemetrics.DiskPath {
	var (
		mu       sync.Mutex
		cached   []nodemetrics.DiskPath
		cachedAt time.Time
	)
	return func() []nodemetrics.DiskPath {
		mu.Lock()
		defer mu.Unlock()
		if cached == nil || time.Since(cachedAt) >= diskPathsRefresh {
			cached, cachedAt = s.nodeDiskPaths(), time.Now()
		}
		return cached
	}
}

// nodeDiskPaths lists the directories this process writes to, by what they
// hold. The snapshot reports the filesystem of each (and of the root), so a
// small tmpfs holding the request body cache shows up next to the disk with
// the logs.
func (s *Server) nodeDiskPaths() []nodemetrics.DiskPath {
	var paths []nodemetrics.DiskPath
	add := func(use, path string) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, nodemetrics.DiskPath{Use: use, Path: path})
		}
	}
	if wd, err := os.Getwd(); err == nil {
		add(nodemetrics.DiskUseWorkdir, wd)
	}
	if cfg := s.liveConfig(nil); cfg != nil {
		if dir, err := util.ResolveAuthDir(cfg.AuthDir); err == nil {
			add(nodemetrics.DiskUseAuth, dir)
		}
		add(nodemetrics.DiskUseLogs, logging.ResolveLogDirectory(cfg))
	}
	add(nodemetrics.DiskUseBodyCache, bodyutil.RequestBodyCacheDir())
	add(nodemetrics.DiskUseUsageSpool, usage.UsageSpoolDir())
	for _, extra := range strings.Split(os.Getenv(nodeDiskPathsEnv), ",") {
		add(nodemetrics.DiskUseExtra, extra)
	}
	return paths
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

// sharedRedisSnapshot reports whether this node's limits, slots and session
// affinity are cluster-wide (the cluster Redis answers) or have fallen back
// to counting on this node. It leaves out the address and the last error,
// which the snapshot would store and show.
func (s *Server) sharedRedisSnapshot() *nodemetrics.SharedRedis {
	if s.clusterRuntime == nil {
		// A single node has no cluster Redis.
		return nil
	}
	status := s.clusterRuntime.Status()
	out := &nodemetrics.SharedRedis{Configured: status.Addr != "", Available: status.Available}
	if !status.Since.IsZero() {
		since := status.Since.UTC()
		out.Since = &since
	}
	return out
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

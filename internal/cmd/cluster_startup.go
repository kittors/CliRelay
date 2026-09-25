package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// migrationLockWait bounds how long a starting node waits for another
	// node's migrations and one-time imports before it gives up.
	migrationLockWait = 10 * time.Minute
	// repairLockWait bounds the wait for another node's post-start repairs.
	// Those repairs are idempotent, so a node that times out skips them.
	repairLockWait = 30 * time.Minute
	// clusterStartTimeout bounds joining the cluster: registration and the
	// bus listener.
	clusterStartTimeout = 30 * time.Second
)

func clusterOptions(cfg *config.Config) cluster.Options {
	return cluster.Options{
		Enabled:   cfg.Cluster.Enabled,
		NodeID:    cfg.Cluster.NodeID,
		Advertise: cfg.Cluster.Advertise,
		DSN:       cfg.Postgres.DSN,
		Version:   buildinfo.Version,
	}
}

// prepareCluster runs before the runtime data stack. In cluster mode it
// tightens TCP keepalives for every database connection opened afterwards
// and installs a follower coordinator, so leader-only work started during
// startup waits for the election. Single-node mode is left untouched.
func prepareCluster(cfg *config.Config) {
	if !cfg.Cluster.Enabled {
		return
	}
	compatdriver.SetDialKeepAlive(cluster.DialKeepAlive())
	cluster.Prepare(clusterOptions(cfg))
}

// startCluster joins the cluster once the runtime database is migrated. In
// single-node mode it installs the single-node coordinator and cannot fail.
func startCluster(cfg *config.Config) (*cluster.Coordinator, error) {
	opts := clusterOptions(cfg)
	opts.DB = usage.RuntimeDB()
	ctx, cancel := context.WithTimeout(context.Background(), clusterStartTimeout)
	defer cancel()
	coordinator, err := cluster.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	if coordinator.Enabled() {
		log.Infof("cluster: node %s joined (leader=%v, active nodes=%d)",
			coordinator.NodeID(), coordinator.IsLeader(), coordinator.ActiveNodeCount())
	}
	return coordinator, nil
}

// closeClusterOnShutdown leaves the cluster as soon as ctx ends, instead of
// after the request drain that follows, so leadership moves to a peer at
// once. The returned stop ends the watch.
func closeClusterOnShutdown(ctx context.Context, coordinator *cluster.Coordinator) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			coordinator.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// withRuntimeMigrationLock runs fn, the whole runtime data stack
// initialisation, under the cluster-wide migration lock. Nodes starting
// together would otherwise race through migrations (one of them failing on
// "migration is dirty"), the YAML imports and the backfills. A lone node
// takes the lock uncontended.
//
// The runtime pool is opened inside fn, so the lock is held on a short-lived
// pool of its own.
func withRuntimeMigrationLock(cfg *config.Config, fn func() error) error {
	dsn := strings.TrimSpace(cfg.Postgres.DSN)
	if dsn == "" {
		// Let initialisation report the missing DSN as it always has.
		return fn()
	}
	lockDB, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		return fmt.Errorf("open migration lock connection: %w", err)
	}
	defer lockDB.Close()
	lockDB.SetMaxOpenConns(1)

	ran := false
	run := func(context.Context) error {
		ran = true
		return fn()
	}
	ctx := context.Background()
	err = cluster.WithSessionLock(ctx, lockDB, cluster.LockMigrate, 0, run)
	if !ran && errors.Is(err, cluster.ErrLockTimeout) {
		log.Infof("postgres: another node is migrating the database; waiting up to %s", migrationLockWait)
		err = cluster.WithSessionLock(ctx, lockDB, cluster.LockMigrate, migrationLockWait, run)
	}
	return err
}

// withRepairLock serialises the post-start repair hooks across nodes. They
// are idempotent: a node that waited out another node's run finds nothing
// left to fix, and one that times out skips them until its next start.
func withRepairLock(fn func() error) error {
	ran := false
	err := cluster.WithSessionLock(context.Background(), usage.RuntimeDB(), cluster.LockMaintenanceRepairs, repairLockWait, func(context.Context) error {
		ran = true
		return fn()
	})
	if !ran && errors.Is(err, cluster.ErrLockTimeout) {
		log.Warnf("usage: another node is still running the post-start repairs after %s; skipping them on this node", repairLockWait)
		return nil
	}
	return err
}

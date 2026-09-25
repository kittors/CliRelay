package cmd

import (
	"context"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	usageSpoolDirName = "usage-spool"
	// usageSpoolStopTimeout bounds how long shutdown waits for queued usage
	// records. With the database up they drain in seconds; with it down they
	// go to the spool without waiting on it.
	usageSpoolStopTimeout = 30 * time.Second
)

// resolveUsageSpoolDir picks where the request log spool lives. The spool only
// helps if it survives a restart, so it is db-resilience.usage-spool-dir or
// CLIRELAY_USAGE_SPOOL_DIR when set, and otherwise a directory in the persistent
// state location util.StateDir picks.
func resolveUsageSpoolDir(cfg *config.Config) string {
	if dir := cfg.DBResilience.UsageSpoolDirOverride(); dir != "" {
		if resolved, err := util.ResolveAuthDir(dir); err == nil && resolved != "" {
			return resolved
		}
		return dir
	}
	return filepath.Join(util.StateDir(cfg.AuthDir), usageSpoolDirName)
}

// startUsageSpool starts the request log spool. A spool that cannot start is
// logged, not fatal: the gateway then behaves as it did before spooling
// existed.
func startUsageSpool(cfg *config.Config) {
	maxBytes, enabled := usage.UsageSpoolMaxBytes(cfg.DBResilience.UsageSpoolMaxSizeMB)
	if !enabled {
		log.Warn("usage: request log spool disabled by db-resilience.usage-spool-max-size-mb; a record the database refuses is dropped")
		return
	}
	dir := resolveUsageSpoolDir(cfg)
	if err := usage.StartUsageSpool(dir, maxBytes); err != nil {
		log.WithError(err).Errorf("usage: request log spool unavailable at %s; a record the database refuses is dropped", dir)
	}
}

func stopUsageSpool() {
	ctx, cancel := context.WithTimeout(context.Background(), usageSpoolStopTimeout)
	defer cancel()
	usage.StopUsageSpool(ctx)
}

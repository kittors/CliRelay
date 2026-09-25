package cmd

import (
	"context"
	"os"
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
// helps if it survives a restart, so the order prefers persistent locations:
//
//  1. db-resilience.usage-spool-dir, or CLIRELAY_USAGE_SPOOL_DIR;
//  2. WRITABLE_PATH, the base the log directory already uses when set;
//  3. a "data" directory next to the auth directory, when it exists. The
//     Docker image mounts /CLIProxyAPI/data as a volume, while /CLIProxyAPI
//     itself belongs to the container and is lost when it is recreated;
//  4. the auth directory's parent;
//  5. the working directory.
func resolveUsageSpoolDir(cfg *config.Config) string {
	if dir := cfg.DBResilience.UsageSpoolDirOverride(); dir != "" {
		if resolved, err := util.ResolveAuthDir(dir); err == nil && resolved != "" {
			return resolved
		}
		return dir
	}
	if base := util.WritablePath(); base != "" {
		return filepath.Join(base, usageSpoolDirName)
	}
	authDir, err := util.ResolveAuthDir(cfg.AuthDir)
	if err != nil || authDir == "" {
		return usageSpoolDirName
	}
	parent := filepath.Dir(authDir)
	if info, err := os.Stat(filepath.Join(parent, "data")); err == nil && info.IsDir() {
		return filepath.Join(parent, "data", usageSpoolDirName)
	}
	return filepath.Join(parent, usageSpoolDirName)
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

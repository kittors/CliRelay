// Command clirelay-hostagent reports a machine that runs no CliRelay node to
// the cluster's console: the database host above all. SQL can tell the leader
// how big the database is but not how much room is left on the disk under
// it, so the agent runs there and writes the machine part of a node snapshot
// (CPU split into running and stolen time, load, memory, and the
// filesystems of the directories it is given) into the cluster_hosts table of
// the shared PostgreSQL every interval. GET /v0/management/cluster lists the
// rows as "hosts".
//
// It needs only a connection to that database (on the database host, the
// loopback address) and the right to write cluster_hosts; it opens no port.
// A sample systemd unit lives in deploy/cluster/hostagent.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/nodemetrics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

// Exit statuses. The systemd unit lists exitBadConfig in
// RestartPreventExitStatus=, since restarting cannot fix a bad flag or DSN
// file.
const (
	exitOK        = 0
	exitRuntime   = 1
	exitBadConfig = 2
)

const (
	defaultInterval = 15 * time.Second
	// writeTimeout bounds one upsert; the next interval tries again.
	writeTimeout = 5 * time.Second
	// failureLogEvery limits repeated failure logs while the database is
	// away; the first failure and the recovery are always logged.
	failureLogEvery = 5 * time.Minute
	// onceGap separates the two samples of -once, so the CPU figures have a
	// window.
	onceGap = 5 * time.Second
)

func main() {
	os.Exit(run())
}

func run() int {
	var paths pathFlags
	dsnFile := flag.String("dsn-file", "", "file holding the PostgreSQL DSN of the cluster database (required unless -once)")
	hostID := flag.String("host-id", "", "name of this host in the console (default: the host name)")
	role := flag.String("role", cluster.HostRoleDatabase, "what this host is for; the console shows the \"database\" host next to the database")
	interval := flag.Duration("interval", defaultInterval, "how often to write a snapshot")
	once := flag.Bool("once", false, "print one snapshot as JSON and exit, without the database")
	flag.Var(&paths, "path", "a directory whose filesystem to report, as USE=DIR or DIR (repeatable); the root filesystem is always reported")
	flag.Parse()

	logger := newLogger()
	if *interval < time.Second {
		logger.Error("invalid -interval: must be at least 1s", "interval", interval.String())
		return exitBadConfig
	}
	if strings.TrimSpace(*hostID) == "" {
		name, err := os.Hostname()
		if err != nil || strings.TrimSpace(name) == "" {
			logger.Error("no -host-id given and the host name is unknown", "error", err)
			return exitBadConfig
		}
		*hostID = name
	}

	collector := nodemetrics.New(nodemetrics.Options{Interval: *interval})
	collector.SetSources(nodemetrics.Sources{
		Version:   buildinfo.Version,
		DiskPaths: func() []nodemetrics.DiskPath { return paths },
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		return printOnce(ctx, collector)
	}

	dsn, err := readDSN(*dsnFile)
	if err != nil {
		logger.Error("cannot read the DSN file", "error", err)
		return exitBadConfig
	}
	db, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		logger.Error("invalid DSN", "error", err)
		return exitBadConfig
	}
	defer func() { _ = db.Close() }()
	// One upsert at a time: a single connection is plenty.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)

	logger.Info("reporting", "host_id", *hostID, "role", *role, "interval", interval.String(), "paths", paths.String())
	agent := &agent{db: db, collector: collector, hostID: *hostID, role: *role, logger: logger}
	agent.loop(ctx, *interval)
	return exitOK
}

// agent writes the host's snapshot every interval.
type agent struct {
	db        *sql.DB
	collector *nodemetrics.Collector
	hostID    string
	role      string
	logger    *slog.Logger

	failingSince time.Time
	lastFailLog  time.Time
}

func (a *agent) loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		a.report(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *agent) report(ctx context.Context) {
	snapshot, ok := a.collector.Sample(ctx)
	if !ok {
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = cluster.UpsertHost(writeCtx, a.db, a.hostID, a.role, snapshot.Version, raw)
	if ctx.Err() != nil {
		return
	}
	now := time.Now()
	if err != nil {
		if a.failingSince.IsZero() {
			a.failingSince = now
		}
		if a.lastFailLog.IsZero() || now.Sub(a.lastFailLog) >= failureLogEvery {
			a.lastFailLog = now
			a.logger.Warn("cannot write the snapshot", "error", err, "failing_since", a.failingSince.Format(time.RFC3339), "hint", failureHint(err))
		}
		return
	}
	if !a.failingSince.IsZero() {
		a.logger.Info("writing snapshots again", "failed_for", now.Sub(a.failingSince).Round(time.Second).String())
		a.failingSince, a.lastFailLog = time.Time{}, time.Time{}
	}
}

// failureHint names the usual cause of a write failure.
func failureHint(err error) string {
	text := err.Error()
	switch {
	case strings.Contains(text, "cluster_hosts") && strings.Contains(text, "does not exist"):
		return "the nodes create cluster_hosts when they start; upgrade them first"
	case strings.Contains(text, "permission denied"):
		return "the role needs SELECT, INSERT and UPDATE on cluster_hosts"
	default:
		return ""
	}
}

// printOnce takes two samples onceGap apart, so the CPU figures cover a
// window, and prints the second.
func printOnce(ctx context.Context, collector *nodemetrics.Collector) int {
	if _, ok := collector.Sample(ctx); !ok {
		return exitRuntime
	}
	select {
	case <-ctx.Done():
		return exitRuntime
	case <-time.After(onceGap):
	}
	snapshot, ok := collector.Sample(ctx)
	if !ok {
		return exitRuntime
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshot); err != nil {
		return exitRuntime
	}
	return exitOK
}

// readDSN reads the DSN from a file, so it stays out of the command line
// (visible in ps) and the unit file (a systemd credential holds it).
func readDSN(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("-dsn-file is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dsn := strings.TrimSpace(string(data))
	if dsn == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return dsn, nil
}

// pathFlags collects -path flags: USE=DIR, or DIR alone for the use "extra".
type pathFlags []nodemetrics.DiskPath

func (p *pathFlags) String() string {
	if p == nil {
		return ""
	}
	parts := make([]string, 0, len(*p))
	for _, path := range *p {
		parts = append(parts, path.Use+"="+path.Path)
	}
	return strings.Join(parts, ",")
}

func (p *pathFlags) Set(value string) error {
	use, dir, found := strings.Cut(value, "=")
	if !found {
		use, dir = nodemetrics.DiskUseExtra, value
	}
	use, dir = strings.TrimSpace(use), strings.TrimSpace(dir)
	if use == "" || dir == "" {
		return fmt.Errorf("want USE=DIR or DIR, got %q", value)
	}
	if !strings.HasPrefix(dir, "/") {
		return fmt.Errorf("%q is not an absolute path", dir)
	}
	*p = append(*p, nodemetrics.DiskPath{Use: use, Path: dir})
	return nil
}

// newLogger writes text logs to stderr. Under systemd the journal already
// timestamps every line, so the duplicate time field is dropped there.
func newLogger() *slog.Logger {
	options := &slog.HandlerOptions{}
	if os.Getenv("JOURNAL_STREAM") != "" {
		options.ReplaceAttr = func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, options))
}

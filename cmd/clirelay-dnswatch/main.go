// Command clirelay-dnswatch keeps the Cloudflare A records of the CliRelay
// hostnames pointed at the application nodes that pass their readiness probe.
//
// It runs on a third (arbiter) machine: a node cannot judge whether clients
// can reach it. A sample systemd unit and config live in deploy/cluster/dnswatch.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/ops/dnswatch"
)

const defaultConfigPath = "/etc/clirelay-dnswatch/dnswatch.yaml"

// Exit statuses. The systemd unit lists exitBadConfig in
// RestartPreventExitStatus=, since restarting cannot fix a broken config.
const (
	exitOK        = 0
	exitRuntime   = 1
	exitBadConfig = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", defaultConfigPath, "path to the YAML config")
	checkOnly := flag.Bool("check", false, "validate the config and the token file, print the effective settings, and exit")
	flag.Parse()

	logger := newLogger()
	cfg, err := dnswatch.LoadConfig(*configPath)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return exitBadConfig
	}
	token, err := dnswatch.ReadAPIToken(cfg.Cloudflare.APITokenFile)
	if err != nil {
		logger.Error("cannot load the Cloudflare API token", "error", err)
		return exitBadConfig
	}
	if *checkOnly {
		logger.Info("configuration ok", cfg.LogAttrs()...)
		return exitOK
	}

	watcher, err := dnswatch.New(cfg, token, dnswatch.Options{Logger: logger})
	if err != nil {
		logger.Error("cannot start", "error", err)
		return exitBadConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := watcher.Run(ctx); err != nil {
		logger.Error("dnswatch stopped", "error", err)
		return exitRuntime
	}
	return exitOK
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

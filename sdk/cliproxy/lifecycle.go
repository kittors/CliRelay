package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// Run starts the service and blocks until the context is cancelled or the server stops.
// It owns the startup sequence for the HTTP server, watcher, refresh worker, and
// OpenRouter sync loop, then waits for cancellation or server termination.
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	usage.StartDefault(ctx)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGracePeriod())
	defer shutdownCancel()
	defer func() {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Errorf("service shutdown returned error: %v", err)
		}
	}()

	if err := s.loadInitialState(ctx); err != nil {
		return err
	}

	s.configureServer(ctx)

	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	s.startServerLoop()
	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	if err := s.startWatcher(ctx); err != nil {
		return err
	}

	if s.coreManager != nil {
		interval := 15 * time.Minute
		s.coreManager.StartAutoRefresh(context.WithoutCancel(ctx), interval)
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
	}
	s.startOpenRouterModelSync(ctx)

	select {
	case <-ctx.Done():
		log.Debug("service context cancelled, shutting down...")
		return ctx.Err()
	case err := <-s.serverErr:
		return err
	}
}

func (s *Service) loadInitialState(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.ensureAuthDir(); err != nil {
		return err
	}

	s.applyDBBackedRuntimeSettings(s.cfg)
	s.applyRetryConfig(s.cfg)
	if s.coreManager != nil {
		s.coreManager.SetConfig(s.cfg)
		s.coreManager.SetOAuthModelAlias(s.cfg.OAuthModelAlias)
		if errLoad := s.coreManager.Load(ctx); errLoad != nil {
			log.Warnf("failed to load auth store: %v", errLoad)
		}
		s.syncConfigDerivedAuths(s.cfg)
		// Fetch upstream model lists now, so the first answers overlap the
		// registration pass below instead of following it; the lists re-register
		// their credentials once that pass is done.
		s.startProviderDiscovery(ctx)
		for _, auth := range s.coreManager.List() {
			if auth == nil || auth.ID == "" {
				continue
			}
			s.ensureExecutorsForAuth(auth)
			s.registerModelsForAuth(ctx, auth)
		}
		s.markProviderDiscoveryRegistrationReady()
	}

	if _, err := s.tokenProvider.Load(ctx, s.cfg); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	if _, err := s.apiKeyProvider.Load(ctx, s.cfg); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (s *Service) startServerLoop() {
	if s == nil || s.server == nil {
		return
	}
	s.serverErr = launchServiceServerLoop(s.server)

	time.Sleep(100 * time.Millisecond)
	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)
}

// Shutdown gracefully stops background workers and the HTTP server.
// It is idempotent and drains every service-owned background worker before the
// process returns control to the caller.
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		if s.watcherCancel != nil {
			s.watcherCancel()
		}
		if s.coreManager != nil {
			s.coreManager.StopAutoRefresh()
		}
		s.stopProviderDiscovery()
		if s.watcher != nil {
			if err := s.watcher.Stop(); err != nil {
				log.Errorf("failed to stop file watcher: %v", err)
				shutdownErr = err
			}
		}
		if s.wsGateway != nil {
			if err := s.wsGateway.Stop(ctx); err != nil {
				log.Errorf("failed to stop websocket gateway: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.authQueueStop != nil {
			s.authQueueStop()
			s.authQueueWG.Wait()
			s.authQueueStop = nil
		}

		if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
			log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
			if shutdownErr == nil {
				shutdownErr = errShutdownPprof
			}
		}

		if s.server != nil {
			// Honour the caller's deadline instead of clamping it. Wrapping the
			// incoming context in a fresh 30s timeout here silently overrode
			// whatever budget the caller had granted, so widening the window in
			// Run alone would have changed nothing: this was the limit that
			// actually cut off in-flight requests.
			stopCtx := ctx
			if _, hasDeadline := ctx.Deadline(); !hasDeadline {
				var cancel context.CancelFunc
				stopCtx, cancel = context.WithTimeout(ctx, shutdownGracePeriod())
				defer cancel()
			}
			if err := s.server.Stop(stopCtx); err != nil {
				log.Errorf("error stopping API server: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}

		usage.StopDefault()
	})
	return shutdownErr
}

func (s *Service) ensureAuthDir() error {
	if s == nil || s.cfg == nil {
		return nil
	}
	return ensureServiceAuthDir(s.cfg.AuthDir)
}

// shutdownGracePeriod bounds how long a terminating process waits for requests
// already in flight to finish.
//
// This is a proxy in front of LLMs: a single streamed answer routinely runs for
// minutes. The previous fixed 30s meant a blue-green deploy cut off any request
// still streaming 30 seconds after the old slot was signalled, which is a
// user-visible failure even though the deploy itself reported success. The
// window now defaults to five minutes and is overridable, so an operator can
// match it to the systemd TimeoutStopSec on the host.
//
// Note this is an upper bound, not a delay: Shutdown returns as soon as the
// last in-flight request completes.
func shutdownGracePeriod() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CLIRELAY_SHUTDOWN_GRACE"))
	if raw == "" {
		return defaultShutdownGracePeriod
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		log.Warnf("cliproxy: ignoring invalid CLIRELAY_SHUTDOWN_GRACE %q, using %s", raw, defaultShutdownGracePeriod)
		return defaultShutdownGracePeriod
	}
	return parsed
}

const defaultShutdownGracePeriod = 5 * time.Minute

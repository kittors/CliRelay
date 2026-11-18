package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

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
		// The pass below registers every credential from lists already at hand —
		// the compiled-in catalogs and the lists the previous process saved — and
		// never waits on an upstream, so the listener comes up whether or not the
		// upstreams answer. Fetching starts first so the first live answers overlap
		// the pass; each re-registers its credentials once it arrives.
		s.loadModelLists()
		s.loadProviderDiscovery()
		s.startProviderDiscovery(ctx)
		s.startLiveModelLists(ctx)
		started := time.Now()
		registered := 0
		for _, auth := range s.coreManager.List() {
			if auth == nil || auth.ID == "" {
				continue
			}
			s.ensureExecutorsForAuth(auth)
			s.registerModelsForAuth(ctx, auth)
			registered++
		}
		s.markProviderDiscoveryRegistrationReady()
		log.Infof("model registration: registered %d credentials in %s; upstream model lists refresh in the background", registered, time.Since(started).Round(time.Millisecond))
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

// shutdownGracePeriod bounds how long a terminating process waits for requests
// already in flight to finish.
//
// This is a proxy in front of LLMs: a single streamed answer routinely runs for
// minutes. The previous fixed 30s meant a blue-green deploy cut off any request
// still streaming 30 seconds after the old slot was signalled, which is a
// user-visible failure even though the deploy itself reported success. The
// window now defaults to five minutes and is overridable. It covers the drain
// only: writing the queued usage records and closing the database run after it
// ends, so the host's systemd TimeoutStopSec has to be longer than the window,
// not equal to it (scripts/deploy-blue-green.sh adds
// SHUTDOWN_STOP_HEADROOM_SECONDS).
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

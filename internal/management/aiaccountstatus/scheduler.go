package aiaccountstatus

import (
	"context"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Scheduler re-probes every tenant's accounts on a fixed interval.
//
// Without it a quota snapshot only advances when a panel session asks for one.
// That is enough while the proxy is the only thing spending the allowance, but
// Grok bills its weekly pool across surfaces the proxy never sees: an account
// drained by Grok Chat on the web reads as untouched here until somebody opens
// the page. Everything the panel's refresh button does is reused, including the
// per-account min-gap and the in-flight dedupe, so a scheduled round overlapping
// a manual one costs no additional upstream calls.
type Scheduler struct {
	serviceFor func() *Service
	authFor    func() *coreauth.Manager
	interval   time.Duration
	startDelay time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewScheduler builds a scheduler. Both accessors are called per round rather
// than captured once, because a config reload replaces the status service.
func NewScheduler(serviceFor func() *Service, authFor func() *coreauth.Manager, interval, startDelay time.Duration) *Scheduler {
	return &Scheduler{serviceFor: serviceFor, authFor: authFor, interval: interval, startDelay: startDelay}
}

// Start runs rounds until Stop. Restarting an already-running scheduler stops
// the previous loop first, which is what a config reload needs.
func (s *Scheduler) Start() {
	if s == nil || s.serviceFor == nil || s.authFor == nil || s.interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	previous := s.cancel
	s.cancel = cancel
	s.mu.Unlock()
	if previous != nil {
		previous()
	}

	go s.loop(ctx)
	log.Infof("ai account status: background refresh every %s", s.interval)
}

// Stop halts the loop. Safe to call when never started.
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Scheduler) loop(ctx context.Context) {
	if s.startDelay > 0 {
		timer := time.NewTimer(s.startDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.runRound()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) runRound() {
	service := s.serviceFor()
	if service == nil {
		return
	}
	tenants := tenantIDsWithAccounts(s.authFor())
	if len(tenants) == 0 {
		return
	}
	var accepted, skipped int
	for _, tenantID := range tenants {
		// force=false on purpose: an account probed moments ago by the panel is
		// reported as skipped rather than probed twice.
		result := service.StartRefresh(tenantID, RefreshRequest{})
		accepted += result.Accepted
		skipped += len(result.Skipped)
	}
	if accepted > 0 {
		log.Debugf("ai account status: scheduled refresh queued %d account(s) across %d tenant(s), %d already fresh",
			accepted, len(tenants), skipped)
	}
}

// tenantIDsWithAccounts lists the tenants that own at least one credential.
// Tenants without credentials are skipped so an empty tenant does not cost a
// job per round.
func tenantIDsWithAccounts(manager *coreauth.Manager) []string {
	if manager == nil {
		return nil
	}
	seen := make(map[string]struct{})
	tenants := make([]string, 0, 1)
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		tenantID := strings.TrimSpace(auth.TenantID)
		if _, ok := seen[tenantID]; ok {
			continue
		}
		seen[tenantID] = struct{}{}
		tenants = append(tenants, tenantID)
	}
	return tenants
}

package warmup

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Service encapsulates warmup execution, scheduling, and state reporting.
type Service struct {
	cfg       *config.Config
	authMgr   *coreauth.Manager
	registry  *DriverRegistry
	queue     *StaggeredQueue
	scheduler *PolicyScheduler

	mu         sync.RWMutex
	recentLogs []*Result
}

func NewService(cfg *config.Config, authMgr *coreauth.Manager) *Service {
	registry := NewDriverRegistry()
	registry.Register(NewAntigravityDriver(cfg))
	registry.Register(NewCodexDriver(cfg))

	svc := &Service{
		cfg:      cfg,
		authMgr:  authMgr,
		registry: registry,
	}

	queue := NewStaggeredQueue(
		StaggeredQueueOptions{
			MaxConcurrency: 2,
			MinJitter:      1 * time.Second,
			MaxJitter:      4 * time.Second,
		},
		registry,
		svc.recordResult,
	)
	svc.queue = queue

	scheduler := NewPolicyScheduler(queue, registry, authMgr)
	svc.scheduler = scheduler

	return svc
}

func (s *Service) Start() {
	s.scheduler.Start()
}

func (s *Service) Stop() {
	s.scheduler.Stop()
}

func (s *Service) recordResult(r *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recentLogs = append([]*Result{r}, s.recentLogs...)
	if len(s.recentLogs) > 100 {
		s.recentLogs = s.recentLogs[:100]
	}
}

// GetAccountTargets returns available warmup targets for a specific credential.
func (s *Service) GetAccountTargets(authID string) ([]Target, error) {
	if s.authMgr == nil {
		return nil, fmt.Errorf("auth manager unavailable")
	}
	auth, ok := s.authMgr.GetByID(authID)
	if !ok || auth == nil {
		return nil, fmt.Errorf("auth %s not found", authID)
	}

	driver, okDriver := s.registry.Get(auth.Provider)
	if !okDriver {
		return nil, fmt.Errorf("no warmup driver for provider %s", auth.Provider)
	}

	return driver.GetTargets(auth), nil
}

// WarmupSingleAccount triggers an immediate warmup for one pool of an account.
func (s *Service) WarmupSingleAccount(ctx context.Context, authID string, poolID string) (*Result, error) {
	if s.authMgr == nil {
		return nil, fmt.Errorf("auth manager unavailable")
	}
	auth, ok := s.authMgr.GetByID(authID)
	if !ok || auth == nil {
		return nil, fmt.Errorf("auth %s not found", authID)
	}

	driver, okDriver := s.registry.Get(auth.Provider)
	if !okDriver {
		return nil, fmt.Errorf("no warmup driver for provider %s", auth.Provider)
	}

	targets := driver.GetTargets(auth)
	var matched *Target
	for _, t := range targets {
		if poolID == "" || t.PoolID == poolID {
			matched = &t
			break
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("target pool %s not found for provider %s", poolID, auth.Provider)
	}

	res, err := driver.ExecuteWarmup(ctx, auth, *matched)
	if res != nil {
		s.recordResult(res)
	}
	return res, err
}

func (s *Service) GetMetrics() QueueMetrics {
	return s.queue.GetMetrics()
}

func (s *Service) GetRecentLogs() []*Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Result, len(s.recentLogs))
	copy(out, s.recentLogs)
	return out
}

func (s *Service) AddPolicy(p Policy) {
	s.scheduler.AddPolicy(p)
}

func (s *Service) GetPolicies(tenantID string) []Policy {
	return s.scheduler.GetPolicies(tenantID)
}

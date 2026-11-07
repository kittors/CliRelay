package warmup

import (
	"context"
	"errors"
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

// ErrAuthNotFound is returned for an account the caller's tenant does not own,
// whether or not it exists: telling the two apart would let a tenant probe for
// other tenants' credential ids.
var ErrAuthNotFound = errors.New("auth not found")

// authForTenant returns the account authID if tenantID owns it. A warmup sends a
// real upstream request on the account's own credential, so it is limited to the
// accounts the tenant manages; platform accounts shared with a tenant are only
// lent for routing and are warmed by the platform, not by the tenant.
func (s *Service) authForTenant(tenantID, authID string) (*coreauth.Auth, error) {
	if s.authMgr == nil {
		return nil, fmt.Errorf("auth manager unavailable")
	}
	auth, ok := s.authMgr.GetByID(authID)
	if !ok || auth == nil || coreauth.NormalizedTenantID(auth.TenantID) != coreauth.NormalizedTenantID(tenantID) {
		return nil, fmt.Errorf("%w: %s", ErrAuthNotFound, authID)
	}
	return auth, nil
}

// GetAccountTargets returns available warmup targets for a credential of tenantID.
func (s *Service) GetAccountTargets(tenantID, authID string) ([]Target, error) {
	auth, err := s.authForTenant(tenantID, authID)
	if err != nil {
		return nil, err
	}

	driver, okDriver := s.registry.Get(auth.Provider)
	if !okDriver {
		return nil, fmt.Errorf("no warmup driver for provider %s", auth.Provider)
	}

	return driver.GetTargets(auth), nil
}

// WarmupSingleAccount triggers an immediate warmup for one pool of an account of tenantID.
func (s *Service) WarmupSingleAccount(ctx context.Context, tenantID, authID string, poolID string) (*Result, error) {
	auth, err := s.authForTenant(tenantID, authID)
	if err != nil {
		return nil, err
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

// WarmupBatchAccounts triggers immediate staggered warmup for accounts of
// tenantID; ids the tenant does not own are skipped like unknown ones.
func (s *Service) WarmupBatchAccounts(ctx context.Context, tenantID string, authIDs []string, poolID string) (int, error) {
	if s.authMgr == nil {
		return 0, fmt.Errorf("auth manager unavailable")
	}
	if len(authIDs) == 0 {
		return 0, nil
	}

	tasks := make([]Task, 0, len(authIDs))
	now := time.Now()
	for _, id := range authIDs {
		auth, err := s.authForTenant(tenantID, id)
		if err != nil {
			continue
		}
		driver, okDriver := s.registry.Get(auth.Provider)
		if !okDriver {
			continue
		}
		targets := driver.GetTargets(auth)
		for _, t := range targets {
			if poolID == "" || t.PoolID == poolID {
				tasks = append(tasks, Task{
					ID:        auth.ID + ":" + t.PoolID,
					Auth:      auth,
					Target:    t,
					CreatedAt: now,
				})
			}
		}
	}

	if len(tasks) > 0 {
		s.queue.Dispatch(ctx, tasks)
	}
	return len(tasks), nil
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

// SetPolicyStore persists policies in store; set it before Start.
func (s *Service) SetPolicyStore(store PolicyStore) {
	s.scheduler.SetStore(store)
}

// SetLeaderCheck overrides which node evaluates policies; the default is the
// cluster leader.
func (s *Service) SetLeaderCheck(isLeader func() bool) {
	s.scheduler.SetLeaderCheck(isLeader)
}

// SavePolicy creates or replaces a policy, persisting it when a store is set.
func (s *Service) SavePolicy(ctx context.Context, p Policy) error {
	return s.scheduler.SavePolicy(ctx, p)
}

// ListPolicies returns the policies of tenantID as every node sees them.
func (s *Service) ListPolicies(ctx context.Context, tenantID string) ([]Policy, error) {
	return s.scheduler.ListPolicies(ctx, tenantID)
}

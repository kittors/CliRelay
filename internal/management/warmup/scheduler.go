package warmup

import (
	"context"
	"math/rand"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// PolicyScheduler evaluates warmup policies on tick and schedules tasks into StaggeredQueue.
type PolicyScheduler struct {
	queue    *StaggeredQueue
	registry *DriverRegistry
	authMgr  *coreauth.Manager

	mu       sync.RWMutex
	policies map[string]*Policy

	tickerInterval time.Duration
	stopChan       chan struct{}
	running        bool

	nowFunc func() time.Time // Injectable time for tests
}

func NewPolicyScheduler(queue *StaggeredQueue, registry *DriverRegistry, authMgr *coreauth.Manager) *PolicyScheduler {
	return &PolicyScheduler{
		queue:          queue,
		registry:       registry,
		authMgr:        authMgr,
		policies:       make(map[string]*Policy),
		tickerInterval: 1 * time.Minute,
		stopChan:       make(chan struct{}),
		nowFunc:        time.Now,
	}
}

func (s *PolicyScheduler) SetNowFunc(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nowFunc = fn
}

func (s *PolicyScheduler) AddPolicy(p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[p.ID] = &p
}

func (s *PolicyScheduler) GetPolicies(tenantID string) []Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Policy, 0, len(s.policies))
	for _, p := range s.policies {
		if tenantID == "" || p.TenantID == tenantID {
			out = append(out, *p)
		}
	}
	return out
}

func (s *PolicyScheduler) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	go s.loop()
}

func (s *PolicyScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	close(s.stopChan)
}

func (s *PolicyScheduler) loop() {
	ticker := time.NewTicker(s.tickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.EvaluateTick(context.Background())
		}
	}
}

// EvaluateTick evaluates all policies at the current point in time.
func (s *PolicyScheduler) EvaluateTick(ctx context.Context) {
	now := s.nowFunc()

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, policy := range s.policies {
		if !policy.Enabled {
			policy.Status = PolicyStatusDisabled
			continue
		}

		// 1. Check StartAt
		if policy.StartAt != nil && now.Before(*policy.StartAt) {
			policy.Status = PolicyStatusPending
			policy.NextRunAt = policy.StartAt
			continue
		}

		// 2. Check StopAt
		if policy.StopAt != nil && now.After(*policy.StopAt) {
			policy.Status = PolicyStatusCompleted
			policy.Enabled = false
			continue
		}

		// 3. Check Daily Active Window (Quiet Hours)
		if !policy.DailyWindow.IsWithin(now) {
			policy.Status = PolicyStatusQuietHours
			continue
		}

		// 4. Check if due for execution
		due := false
		if policy.LastRunAt == nil {
			due = true
		} else if policy.NextRunAt != nil && !now.Before(*policy.NextRunAt) {
			due = true
		} else if policy.IntervalSeconds > 0 && now.Sub(*policy.LastRunAt) >= time.Duration(policy.IntervalSeconds)*time.Second {
			due = true
		}

		if due {
			policy.Status = PolicyStatusActive
			s.triggerPolicy(ctx, policy, now)
		}
	}
}

func (s *PolicyScheduler) triggerPolicy(ctx context.Context, policy *Policy, now time.Time) {
	policy.TotalRuns++
	policy.LastRunAt = &now

	interval := time.Duration(policy.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Hour
	}
	next := now.Add(interval)
	policy.NextRunAt = &next

	if s.authMgr == nil {
		return
	}

	auths := s.authMgr.List()
	matchingAuths := make([]*coreauth.Auth, 0)
	for _, a := range auths {
		if a == nil || a.Unavailable {
			continue
		}
		if len(policy.Providers) > 0 {
			match := false
			for _, p := range policy.Providers {
				if a.Provider == p {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		matchingAuths = append(matchingAuths, a)
	}

	if len(matchingAuths) == 0 {
		return
	}

	// Prepare staggered tasks
	tasks := make([]Task, 0)
	staggerDuration := time.Duration(policy.StaggerMinutes) * time.Minute
	if staggerDuration <= 0 {
		staggerDuration = 10 * time.Minute
	}

	totalAccounts := len(matchingAuths)
	for i, a := range matchingAuths {
		driver, ok := s.registry.Get(a.Provider)
		if !ok {
			continue
		}
		targets := driver.GetTargets(a)
		for _, target := range targets {
			if len(policy.PoolIDs) > 0 {
				match := false
				for _, poolID := range policy.PoolIDs {
					if poolID == target.PoolID {
						match = true
						break
					}
				}
				if !match {
					continue
				}
			}

			// Compute staggered delay across staggerDuration
			var delay time.Duration
			if totalAccounts > 1 {
				step := staggerDuration / time.Duration(totalAccounts)
				jitter := time.Duration(rand.Int63n(int64(step / 2)))
				delay = step*time.Duration(i) + jitter
			}

			tasks = append(tasks, Task{
				ID:        a.ID + ":" + target.PoolID,
				TenantID:  policy.TenantID,
				Auth:      a,
				Target:    target,
				Delay:     delay,
				CreatedAt: now,
			})
		}
	}

	log.Infof("warmup: policy %s (%s) triggered %d task(s) spread across %v",
		policy.ID, policy.Name, len(tasks), staggerDuration)

	s.queue.Dispatch(ctx, tasks)
}

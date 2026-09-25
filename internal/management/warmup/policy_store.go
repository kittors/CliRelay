package warmup

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// StoredPolicy is a persisted policy with the revision of its last operator
// edit.
type StoredPolicy struct {
	Policy   Policy
	Revision int64
}

// PolicyStore persists warmup policies. Without one, policies live only in
// the scheduler's memory and are lost on restart; with one they survive
// restarts and every node of a cluster sees the same set.
type PolicyStore interface {
	// ListPolicies returns the policies of tenantID, or of every tenant
	// when tenantID is empty.
	ListPolicies(ctx context.Context, tenantID string) ([]StoredPolicy, error)
	// SavePolicy stores an operator edit and returns its revision.
	SavePolicy(ctx context.Context, p Policy) (int64, error)
	// SaveRunState stores the scheduler's bookkeeping for p (status, run
	// times, run count, and Enabled once StopAt passes) unless an operator
	// edited p after revision was read. It reports whether it wrote.
	SaveRunState(ctx context.Context, p Policy, revision int64) (bool, error)
}

const storeTimeout = 10 * time.Second

func policyKey(tenantID, id string) string { return tenantID + "\x00" + id }

// SetStore makes the scheduler persist policies in store and reload them
// before every evaluation, which is how an edit made on another node reaches
// the node that schedules.
func (s *PolicyScheduler) SetStore(store PolicyStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

// SetLeaderCheck makes evaluation run only while isLeader reports true. A
// cluster warms each account once, from the leader; a single node is always
// the leader.
func (s *PolicyScheduler) SetLeaderCheck(isLeader func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.isLeader = isLeader
}

func defaultLeaderCheck() bool { return cluster.Default().IsLeader() }

func (s *PolicyScheduler) shouldEvaluate() bool {
	s.mu.RLock()
	isLeader := s.isLeader
	s.mu.RUnlock()
	return isLeader == nil || isLeader()
}

// SavePolicy stores p, persisting it first when a store is set so that an
// edit the store refused is not applied either.
func (s *PolicyScheduler) SavePolicy(ctx context.Context, p Policy) error {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	revision := int64(0)
	if store != nil {
		rev, err := store.SavePolicy(ctx, p)
		if err != nil {
			return err
		}
		revision = rev
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := policyKey(p.TenantID, p.ID)
	s.policies[key] = &p
	s.revisions[key] = revision
	return nil
}

// ListPolicies returns the policies of tenantID (all tenants when empty),
// read from the store when there is one so every node answers alike.
func (s *PolicyScheduler) ListPolicies(ctx context.Context, tenantID string) ([]Policy, error) {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		return s.GetPolicies(tenantID), nil
	}
	stored, err := store.ListPolicies(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Policy, 0, len(stored))
	for _, sp := range stored {
		out = append(out, sp.Policy)
	}
	return out, nil
}

// reload replaces the in-memory policies with the stored ones. It reports
// false when the store could not be read; evaluating a stale set could then
// run a policy twice, so the caller skips the tick.
//
// Run bookkeeping held in memory wins over the stored copy when the policy
// was not edited since and memory saw a later run: that is a write-back that
// failed, and dropping it would run the policy again next minute.
func (s *PolicyScheduler) reload(ctx context.Context) bool {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		return true
	}
	loadCtx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	stored, err := store.ListPolicies(loadCtx, "")
	if err != nil {
		log.WithError(err).Warn("warmup: failed to load policies; skipping this evaluation")
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	policies := make(map[string]*Policy, len(stored))
	revisions := make(map[string]int64, len(stored))
	for _, sp := range stored {
		p := sp.Policy
		key := policyKey(p.TenantID, p.ID)
		if current := s.policies[key]; current != nil && s.revisions[key] == sp.Revision && ranLater(current, &p) {
			p = *current
		}
		policies[key] = &p
		revisions[key] = sp.Revision
	}
	s.policies = policies
	s.revisions = revisions
	return true
}

func ranLater(a, b *Policy) bool {
	if a.LastRunAt == nil {
		return false
	}
	return b.LastRunAt == nil || a.LastRunAt.After(*b.LastRunAt)
}

type runState struct {
	status    PolicyStatus
	enabled   bool
	lastRunAt time.Time
	nextRunAt time.Time
	totalRuns int64
}

func runStateOf(p *Policy) runState {
	state := runState{status: p.Status, enabled: p.Enabled, totalRuns: p.TotalRuns}
	if p.LastRunAt != nil {
		state.lastRunAt = *p.LastRunAt
	}
	if p.NextRunAt != nil {
		state.nextRunAt = *p.NextRunAt
	}
	return state
}

func (s *PolicyScheduler) runStatesLocked() map[string]runState {
	out := make(map[string]runState, len(s.policies))
	for key, p := range s.policies {
		out[key] = runStateOf(p)
	}
	return out
}

func (a runState) equal(b runState) bool {
	return a.status == b.status && a.enabled == b.enabled && a.totalRuns == b.totalRuns &&
		a.lastRunAt.Equal(b.lastRunAt) && a.nextRunAt.Equal(b.nextRunAt)
}

// changedLocked lists the policies whose bookkeeping differs from before.
func (s *PolicyScheduler) changedLocked(before map[string]runState) []StoredPolicy {
	if s.store == nil {
		return nil
	}
	var out []StoredPolicy
	for key, p := range s.policies {
		if prev, ok := before[key]; ok && prev.equal(runStateOf(p)) {
			continue
		}
		out = append(out, StoredPolicy{Policy: *p, Revision: s.revisions[key]})
	}
	return out
}

func (s *PolicyScheduler) persistRunStates(ctx context.Context, changed []StoredPolicy) {
	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()
	if store == nil {
		return
	}
	for _, sp := range changed {
		saveCtx, cancel := context.WithTimeout(ctx, storeTimeout)
		wrote, err := store.SaveRunState(saveCtx, sp.Policy, sp.Revision)
		cancel()
		switch {
		case err != nil:
			log.WithError(err).WithField("policy_id", sp.Policy.ID).Warn("warmup: failed to persist policy run state")
		case !wrote:
			log.WithField("policy_id", sp.Policy.ID).Debug("warmup: policy was edited during evaluation; keeping the edit")
		}
	}
}

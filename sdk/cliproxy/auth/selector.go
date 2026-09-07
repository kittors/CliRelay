package auth

import (
	"context"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// schedulerDeps carries the shared, cross-request signals a distribution mode
// may consult. All fields are optional: a zero-value selector still works, it
// just falls back to weight-only decisions.
type schedulerDeps struct {
	tracker *selectionPressureTracker
	limiter *AccountConcurrencyLimiter
	quota   QuotaLoadSource
}

func (d *schedulerDeps) observeSelection(auth *Auth, now time.Time) {
	if d == nil || auth == nil {
		return
	}
	d.tracker.observe(auth.ID, now)
}

func (d *schedulerDeps) loadRatio(auth *Auth) float64 {
	if d == nil {
		return 0
	}
	return accountLoadRatio(auth, d.limiter, d.quota)
}

func (d *schedulerDeps) pressure(auth *Auth, now time.Time) float64 {
	if d == nil || auth == nil {
		return 0
	}
	return d.tracker.pressure(auth.ID, now)
}

// RoundRobinSelector distributes requests across candidates in proportion to
// their configured weight (smooth weighted round-robin). With every weight left
// at the default 1 it degenerates to plain round-robin, which is why the name
// is kept for SDK compatibility.
type RoundRobinSelector struct {
	mu       sync.Mutex
	cursors  map[string]int
	weighted map[string]*weightedCursorState
	maxKeys  int
	deps     *schedulerDeps
}

// FillFirstSelector keeps sending traffic to the highest-weighted account until
// that account approaches its ceiling, then moves to the next one. This
// staggers rolling-window subscription caps instead of draining every account
// at the same rate.
//
// The previous implementation returned `available[0]` after sorting by auth ID,
// so which account got burned depended on an opaque internal identifier and it
// only ever moved on after a hard 429.
type FillFirstSelector struct {
	deps *schedulerDeps
}

// fillFirstSwitchLoad is the load ratio at which fill-first considers an
// account "full enough" and moves to the next one. It stops short of 1.0 so the
// switch happens before the upstream starts rejecting requests.
const fillFirstSwitchLoad = 0.9

// Pick selects the next auth for the provider, weighted by configuration.
//
// For gemini-cli virtual auths (identified by the gemini_virtual_parent
// attribute) a two-level round-robin is used instead: first cycling across
// credential groups (parent accounts), then within each group's project auths.
// Weights are not applied there because those entries are projects of one
// credential, not independently billed accounts.
func (s *RoundRobinSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	key := tenantSelectionPrefix(opts.Metadata) + provider + ":" + canonicalModelKey(model)
	s.mu.Lock()
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	limit := s.maxKeys
	if limit <= 0 {
		limit = 4096
	}

	// Check if any available auth has gemini_virtual_parent attribute,
	// indicating gemini-cli virtual auths that should use credential-level polling.
	groups, parentOrder := groupByVirtualParent(available)
	if len(parentOrder) > 1 {
		// Two-level round-robin: first select a credential group, then pick within it.
		groupKey := key + "::group"
		s.ensureCursorKey(groupKey, limit)
		if _, exists := s.cursors[groupKey]; !exists {
			// Seed with a random initial offset so the starting credential is randomized.
			s.cursors[groupKey] = rand.IntN(len(parentOrder))
		}
		groupIndex := s.cursors[groupKey]
		if groupIndex >= 2_147_483_640 {
			groupIndex = 0
		}
		s.cursors[groupKey] = groupIndex + 1

		selectedParent := parentOrder[groupIndex%len(parentOrder)]
		group := groups[selectedParent]

		// Second level: round-robin within the selected credential group.
		innerKey := key + "::cred:" + selectedParent
		s.ensureCursorKey(innerKey, limit)
		innerIndex := s.cursors[innerKey]
		if innerIndex >= 2_147_483_640 {
			innerIndex = 0
		}
		s.cursors[innerKey] = innerIndex + 1
		s.mu.Unlock()
		selected := group[innerIndex%len(group)]
		s.deps.observeSelection(selected, now)
		return selected, nil
	}

	if s.weighted == nil {
		s.weighted = make(map[string]*weightedCursorState)
	}
	weightedKey := weightedSelectionKey(provider, model, opts)
	s.weighted = ensureWeightedState(s.weighted, weightedKey, limit)
	selected := pickWeightedAvailable(s.weighted, weightedKey, available)
	s.mu.Unlock()
	if selected == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	s.deps.observeSelection(selected, now)
	return selected, nil
}

// ensureCursorKey ensures the cursor map has capacity for the given key.
// Must be called with s.mu held.
func (s *RoundRobinSelector) ensureCursorKey(key string, limit int) {
	if _, ok := s.cursors[key]; !ok && len(s.cursors) >= limit {
		s.cursors = make(map[string]int)
	}
}

// groupByVirtualParent groups auths by their gemini_virtual_parent attribute.
// Returns a map of parentID -> auths and a sorted slice of parent IDs for stable iteration.
// Only auths with a non-empty gemini_virtual_parent are grouped; if any auth lacks
// this attribute, nil/nil is returned so the caller falls back to flat round-robin.
func groupByVirtualParent(auths []*Auth) (map[string][]*Auth, []string) {
	if len(auths) == 0 {
		return nil, nil
	}
	groups := make(map[string][]*Auth)
	for _, a := range auths {
		parent := ""
		if a.Attributes != nil {
			parent = strings.TrimSpace(a.Attributes["gemini_virtual_parent"])
		}
		if parent == "" {
			// Non-virtual auth present; fall back to flat round-robin.
			return nil, nil
		}
		groups[parent] = append(groups[parent], a)
	}
	// Collect parent IDs in sorted order for stable cursor indexing.
	parentOrder := make([]string, 0, len(groups))
	for p := range groups {
		parentOrder = append(parentOrder, p)
	}
	sort.Strings(parentOrder)
	return groups, parentOrder
}

// Pick returns the highest-weighted account that still has headroom, falling
// back to the least loaded one when every candidate is near its ceiling.
func (s *FillFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	ordered := make([]*Auth, len(available))
	copy(ordered, available)
	// Highest weight first; ties keep the stable ID order from getAvailableAuths
	// so the fill order is reproducible across restarts.
	sort.SliceStable(ordered, func(i, j int) bool {
		return authSelectionWeight(ordered[i]) > authSelectionWeight(ordered[j])
	})

	var fallback *Auth
	fallbackLoad := 0.0
	for _, candidate := range ordered {
		load := s.deps.loadRatio(candidate)
		if load < fillFirstSwitchLoad {
			s.deps.observeSelection(candidate, now)
			return candidate, nil
		}
		if fallback == nil || load < fallbackLoad {
			fallback = candidate
			fallbackLoad = load
		}
	}
	if fallback == nil {
		fallback = ordered[0]
	}
	s.deps.observeSelection(fallback, now)
	return fallback, nil
}

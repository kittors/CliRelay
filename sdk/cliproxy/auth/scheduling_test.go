package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func weightedAuth(id string, weight int) *Auth {
	auth := &Auth{ID: id, Label: id, Provider: "codex"}
	if weight >= 0 {
		auth.Attributes = map[string]string{selectionWeightAttribute: fmt.Sprint(weight)}
	}
	return auth
}

func distributionOptions(distribution string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{Metadata: map[string]any{
		distributionMetadataKey: distribution,
	}}
}

// The pre-scheduling code kept only the highest priority tier whenever the group
// used session-sticky or fill-first, so an account with no configured priority
// was dropped as soon as any sibling had one. Weights must instead let every
// positive-weight account participate.
func TestGetAvailableAuthsKeepsLowerWeightsInScope(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		weightedAuth("configured", 1),
		{ID: "unconfigured", Label: "unconfigured", Provider: "codex"},
	}

	available, err := getAvailableAuths(auths, "codex", "gpt-5.6-sol", time.Now())
	if err != nil {
		t.Fatalf("getAvailableAuths() error = %v", err)
	}
	if len(available) != 2 {
		t.Fatalf("len(available) = %d, want 2 (an unweighted account still participates)", len(available))
	}
}

func TestGetAvailableAuthsReportsWeightExclusionDistinctly(t *testing.T) {
	t.Parallel()

	auths := []*Auth{weightedAuth("a", 0), weightedAuth("b", 0)}

	_, err := getAvailableAuths(auths, "codex", "gpt-5.6-sol", time.Now())
	if err == nil {
		t.Fatal("getAvailableAuths() error = nil, want a weight-exclusion error")
	}
	authErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("getAvailableAuths() error type = %T, want *Error", err)
	}
	if authErr.Code != "auth_excluded_by_weight" {
		t.Fatalf("error code = %q, want %q", authErr.Code, "auth_excluded_by_weight")
	}
}

// A stored legacy `priority: 0` meant "default tier", not "excluded". Upgrading
// must not take those accounts out of rotation.
func TestLegacyPriorityZeroStillParticipates(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "legacy", Attributes: map[string]string{"priority": "0"}}
	if got := authSelectionWeight(auth); got != defaultSelectionWeight {
		t.Fatalf("authSelectionWeight() = %d, want %d", got, defaultSelectionWeight)
	}
}

func TestMigrateLegacyPrioritiesDropsAllZeroMap(t *testing.T) {
	t.Parallel()

	allZero := map[string]int{"a": 0, "b": 0}
	if got := internalconfig.MigrateLegacyPriorities(allZero); got != nil {
		t.Fatalf("MigrateLegacyPriorities(all zero) = %v, want nil", got)
	}

	mixed := map[string]int{"a": 0, "b": 2}
	if got := internalconfig.MigrateLegacyPriorities(mixed); len(got) != 2 {
		t.Fatalf("MigrateLegacyPriorities(mixed) = %v, want the map preserved", got)
	}
}

func TestWeightedDistributionRespectsShare(t *testing.T) {
	t.Parallel()

	selector := &RoundRobinSelector{}
	auths := []*Auth{weightedAuth("heavy", 3), weightedAuth("light", 1)}

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("weighted"), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[picked.ID]++
	}
	if counts["heavy"] != 300 || counts["light"] != 100 {
		t.Fatalf("counts = %v, want heavy=300 light=100 for a 3:1 weighting", counts)
	}
}

func TestLeastLoadSpreadsAcrossAccounts(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	selector := &LeastLoadSelector{deps: deps}
	auths := []*Auth{
		{ID: "a", Provider: "codex"},
		{ID: "b", Provider: "codex"},
		{ID: "c", Provider: "codex"},
	}

	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("least-load"), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[picked.ID]++
	}
	for id, count := range counts {
		if count < 90 || count > 110 {
			t.Fatalf("counts = %v, account %q is off an even split", counts, id)
		}
	}
}

func TestLeastLoadHonoursWeightRatio(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	selector := &LeastLoadSelector{deps: deps}
	auths := []*Auth{weightedAuth("heavy", 3), weightedAuth("light", 1)}

	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("least-load"), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[picked.ID]++
	}
	// A 3:1 weighting should land near 150/50; allow slack for decay timing.
	if counts["heavy"] < 130 || counts["heavy"] > 170 {
		t.Fatalf("counts = %v, want heavy near 150 for a 3:1 weighting", counts)
	}
}

type stubQuotaSource map[string]float64

func (s stubQuotaSource) QuotaLoadRatio(auth *Auth) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	ratio, ok := s[auth.ID]
	return ratio, ok
}

func TestLeastLoadAvoidsAccountNearQuotaCeiling(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   stubQuotaSource{"nearly-full": 0.95},
	}
	selector := &LeastLoadSelector{deps: deps}
	auths := []*Auth{
		{ID: "fresh", Provider: "codex"},
		{ID: "nearly-full", Provider: "codex"},
	}

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("least-load"), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		counts[picked.ID]++
	}
	if counts["fresh"] <= counts["nearly-full"] {
		t.Fatalf("counts = %v, want the account with quota headroom to take the majority", counts)
	}
}

// fill-first used to return available[0] after sorting by auth ID, so the burn
// order was an internal identifier rather than anything the operator chose.
func TestFillFirstFollowsWeightOrder(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	selector := &FillFirstSelector{deps: deps}
	// "a" sorts first by ID but is configured as the lowest share.
	auths := []*Auth{weightedAuth("a", 1), weightedAuth("z", 9)}

	picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("fill-first"), auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked.ID != "z" {
		t.Fatalf("Pick() = %q, want the highest-weighted account %q", picked.ID, "z")
	}
}

func TestFillFirstMovesOnWhenAccountIsFull(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   stubQuotaSource{"primary": 0.95},
	}
	selector := &FillFirstSelector{deps: deps}
	auths := []*Auth{weightedAuth("primary", 9), weightedAuth("secondary", 1)}

	picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("fill-first"), auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked.ID != "secondary" {
		t.Fatalf("Pick() = %q, want the next account once the primary is full", picked.ID)
	}
}

func stickyOptions(sessionKey string, extra map[string]any) cliproxyexecutor.Options {
	meta := map[string]any{
		cliproxyexecutor.SessionStickyMetadataKey: sessionKey,
		stickyEnabledMetadataKey:                  "true",
	}
	for key, value := range extra {
		meta[key] = value
	}
	return cliproxyexecutor.Options{SourceFormat: "openai", Metadata: meta}
}

// The core of the "one long session burns one account" problem: a binding used
// to survive until the account failed, so a busy conversation kept the same
// upstream until it hit a 429.
func TestSessionStickyReleasesAfterMaxRequests(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	selector := NewSessionStickySelector(&RoundRobinSelector{deps: deps})
	selector.deps = deps
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	opts := stickyOptions("sess-1", map[string]any{stickyMaxRequestsKey: "3"})

	seen := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", opts, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		seen = append(seen, picked.ID)
	}

	// First three requests pin one account, then the binding is released and the
	// distribution places the session on the other one.
	if seen[0] != seen[1] || seen[1] != seen[2] {
		t.Fatalf("picks = %v, want the first three requests pinned to one account", seen)
	}
	if seen[3] == seen[0] {
		t.Fatalf("picks = %v, want a different account after max-requests was reached", seen)
	}
}

func TestSessionStickyReleasesWhenBoundAccountIsLoaded(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   stubQuotaSource{"a": 0.9},
	}
	selector := NewSessionStickySelector(&RoundRobinSelector{deps: deps})
	selector.deps = deps
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}

	// Bind the session to "a" while it still looks healthy.
	bindOpts := stickyOptions("sess-1", nil)
	first, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", bindOpts, auths)
	if err != nil {
		t.Fatalf("Pick() first error = %v", err)
	}
	if first.ID != "a" {
		t.Fatalf("Pick() first = %q, want a", first.ID)
	}

	// Now the same session runs under a group that releases at 80% load.
	releaseOpts := stickyOptions("sess-1", map[string]any{stickyReleaseAtLoadKey: "0.8"})
	second, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", releaseOpts, auths)
	if err != nil {
		t.Fatalf("Pick() second error = %v", err)
	}
	if second.ID != "b" {
		t.Fatalf("Pick() second = %q, want b once the bound account passed the release threshold", second.ID)
	}
}

// Overflow used to reset the whole binding map, moving every live conversation
// onto a different account at the same moment.
func TestSessionStickyOverflowEvictsOnlyColdBindings(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	selector := NewSessionStickySelector(&RoundRobinSelector{deps: deps})
	selector.deps = deps
	selector.maxKeys = 4
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}

	hot := stickyOptions("hot-session", nil)
	hotFirst, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", hot, auths)
	if err != nil {
		t.Fatalf("Pick() hot error = %v", err)
	}

	// Fill the table well past its cap with unrelated sessions, touching the hot
	// session in between so it stays the most recently used entry.
	for i := 0; i < 12; i++ {
		_, _ = selector.Pick(context.Background(), "codex", "gpt-5.6-sol", stickyOptions(fmt.Sprintf("cold-%d", i), nil), auths)
		_, _ = selector.Pick(context.Background(), "codex", "gpt-5.6-sol", hot, auths)
	}

	hotAgain, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", hot, auths)
	if err != nil {
		t.Fatalf("Pick() hot again error = %v", err)
	}
	if hotAgain.ID != hotFirst.ID {
		t.Fatalf("hot session moved from %q to %q; overflow must not evict live bindings", hotFirst.ID, hotAgain.ID)
	}
}

// Stickiness must not swallow the distribution: new sessions are still placed
// by the configured mode.
func TestSessionStickyDelegatesNewSessionsToDistribution(t *testing.T) {
	t.Parallel()

	deps := &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}
	leastLoad := &LeastLoadSelector{deps: deps}
	selector := NewSessionStickySelector(nil)
	selector.deps = deps
	selector.distribution = func(name string) Selector {
		if name != internalconfig.DistributionLeastLoad {
			t.Fatalf("distribution = %q, want least-load to be honoured under stickiness", name)
		}
		return leastLoad
	}
	auths := []*Auth{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}, {ID: "c", Provider: "codex"}}

	counts := map[string]int{}
	for i := 0; i < 60; i++ {
		opts := stickyOptions(fmt.Sprintf("sess-%d", i), map[string]any{
			distributionMetadataKey: internalconfig.DistributionLeastLoad,
		})
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", opts, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", i, err)
		}
		counts[picked.ID]++
	}
	for id, count := range counts {
		if count < 15 || count > 25 {
			t.Fatalf("counts = %v, account %q is off an even split of new sessions", counts, id)
		}
	}
}

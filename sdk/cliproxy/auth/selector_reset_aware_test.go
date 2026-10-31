package auth

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// stubWindowSource serves fixed windows per auth ID and records which model
// each lookup asked about.
type stubWindowSource struct {
	mu      sync.Mutex
	windows map[string][]QuotaWindow
	models  []string
}

func (s *stubWindowSource) QuotaLoadRatio(*Auth) (float64, bool) { return 0, false }

func (s *stubWindowSource) QuotaWindows(auth *Auth, model string) ([]QuotaWindow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append(s.models, model)
	windows, ok := s.windows[auth.ID]
	return windows, ok
}

var resetAwareNow = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

func weekWindow(used float64, renewsIn time.Duration) QuotaWindow {
	return QuotaWindow{Key: "code_week", Span: 7 * 24 * time.Hour, Used: used, ResetAt: resetAwareNow.Add(renewsIn)}
}

func fiveHourWindow(used float64, renewsIn time.Duration) QuotaWindow {
	return QuotaWindow{Key: "code_5h", Span: 5 * time.Hour, Used: used, ResetAt: resetAwareNow.Add(renewsIn)}
}

func TestResetAwareOrder(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name    string
		auths   []*Auth
		windows map[string][]QuotaWindow
		want    []string
	}{
		{
			name:  "the week that renews first goes first",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1), weightedAuth("c", 1)},
			windows: map[string][]QuotaWindow{
				"a": {weekWindow(0.4, 3*day)},
				"b": {weekWindow(0.1, 1*day)},
				"c": {weekWindow(0.0, 5*day)},
			},
			want: []string{"b", "a", "c"},
		},
		{
			name:  "the longest window decides, not the five hours inside it",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1)},
			windows: map[string][]QuotaWindow{
				"a": {fiveHourWindow(0.2, 4*time.Hour), weekWindow(0.3, 1*day)},
				"b": {fiveHourWindow(0.2, 1*time.Hour), weekWindow(0.3, 2*day)},
			},
			want: []string{"a", "b"},
		},
		{
			name:  "renewals in the same hour tie and the next window breaks it",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1)},
			windows: map[string][]QuotaWindow{
				"a": {weekWindow(0.3, 2*day+5*time.Minute), fiveHourWindow(0.2, 4*time.Hour)},
				"b": {weekWindow(0.3, 2*day+50*time.Minute), fiveHourWindow(0.2, 2*time.Hour)},
			},
			want: []string{"b", "a"},
		},
		{
			name:  "accounts that tie keep weight, then ID order",
			auths: []*Auth{weightedAuth("c", 1), weightedAuth("a", 1), weightedAuth("b", 3)},
			windows: map[string][]QuotaWindow{
				"a": {weekWindow(0.3, 2*day)},
				"b": {weekWindow(0.3, 2*day)},
				"c": {weekWindow(0.3, 2*day)},
			},
			want: []string{"b", "a", "c"},
		},
		{
			name:  "an account without data goes after those with room and before nearly spent ones",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1), weightedAuth("c", 1), weightedAuth("d", 1)},
			windows: map[string][]QuotaWindow{
				"b": {weekWindow(0.5, 6*day)},
				"c": {weekWindow(0.95, 1*day)},
				"d": {weekWindow(0.99, 1*time.Hour)},
			},
			want: []string{"b", "a", "c", "d"},
		},
		{
			name:  "nearly spent accounts go by how much they have used",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1)},
			windows: map[string][]QuotaWindow{
				"a": {weekWindow(0.96, 1*time.Hour)},
				"b": {weekWindow(0.91, 6*day)},
			},
			want: []string{"b", "a"},
		},
		{
			name:  "the most used window sets the tier",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1)},
			windows: map[string][]QuotaWindow{
				"a": {fiveHourWindow(0.99, time.Hour), weekWindow(0.1, 1*day)},
				"b": {weekWindow(0.5, 6*day)},
			},
			want: []string{"b", "a"},
		},
		{
			name:  "a window whose renewal is unknown goes after a known one",
			auths: []*Auth{weightedAuth("a", 1), weightedAuth("b", 1)},
			windows: map[string][]QuotaWindow{
				"a": {{Key: "base_quota", Used: 0.1}},
				"b": {weekWindow(0.3, 6*day)},
			},
			want: []string{"b", "a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ranked := rankResetAware(tc.auths, "gpt-5.6-sol", &stubWindowSource{windows: tc.windows})
			got := make([]string, len(ranked))
			for i, standing := range ranked {
				got[i] = standing.auth.ID
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResetAwareWithoutQuotaDataKeepsAFixedOrder(t *testing.T) {
	selector := &ResetAwareSelector{deps: &schedulerDeps{tracker: newSelectionPressureTracker(), limiter: NewAccountConcurrencyLimiter()}}
	auths := []*Auth{weightedAuth("b", 1), weightedAuth("a", 1), weightedAuth("c", 2)}
	for i := 0; i < 3; i++ {
		picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol", distributionOptions("reset-aware"), auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if picked.ID != "c" {
			t.Fatalf("Pick() = %q, want the highest-weighted account while nothing else tells them apart", picked.ID)
		}
	}
}

func TestResetAwarePickSkipsUnavailableAccountsAndAsksForTheBaseModel(t *testing.T) {
	source := &stubWindowSource{windows: map[string][]QuotaWindow{
		"soonest":  {weekWindow(0.2, 2*time.Hour)},
		"next":     {weekWindow(0.2, 30*time.Hour)},
		"excluded": {weekWindow(0.2, time.Hour)},
	}}
	selector := &ResetAwareSelector{deps: &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   source,
	}}
	disabled := weightedAuth("soonest", 1)
	disabled.Disabled = true
	auths := []*Auth{disabled, weightedAuth("next", 1), weightedAuth("excluded", 0)}

	picked, err := selector.Pick(context.Background(), "codex", "gpt-5.6-sol(high)", distributionOptions("reset-aware"), auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if picked.ID != "next" {
		t.Fatalf("Pick() = %q, want the soonest-renewing account that can take the request", picked.ID)
	}
	for _, model := range source.models {
		if model != "gpt-5.6-sol" {
			t.Fatalf("windows were asked for %q, want the model without its thinking suffix", model)
		}
	}
}

const resetAwareTenant = "00000000-0000-0000-0000-0000000000d1"

// A channel group set to reset-aware routes by renewal time, and keeps doing so
// after the selector is replaced on a config reload.
func TestChannelGroupWithResetAwareDistribution(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfigForTenant(resetAwareTenant, &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{{
				Name:       "smart",
				Match:      internalconfig.ChannelGroupMatch{Channels: []string{"Late", "Soon", "Middle"}},
				Scheduling: internalconfig.GroupScheduling{Distribution: "reset-aware"},
			}},
		},
	})
	manager.RegisterExecutorForTenant(resetAwareTenant, &stubExecutor{id: "codex"})
	for _, auth := range []*Auth{
		{ID: "late", TenantID: resetAwareTenant, Label: "Late", Provider: "codex", Status: StatusActive},
		{ID: "soon", TenantID: resetAwareTenant, Label: "Soon", Provider: "codex", Status: StatusActive},
		{ID: "middle", TenantID: resetAwareTenant, Label: "Middle", Provider: "codex", Status: StatusActive},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	manager.SetQuotaLoadSource(&stubWindowSource{windows: map[string][]QuotaWindow{
		"late":   {weekWindow(0.1, 6*24*time.Hour)},
		"soon":   {weekWindow(0.4, 20*time.Hour)},
		"middle": {weekWindow(0.2, 3*24*time.Hour)},
	}})

	pick := func() string {
		t.Helper()
		opts := cliproxyexecutor.Options{Metadata: map[string]any{
			cliproxyexecutor.TenantMetadataKey:     resetAwareTenant,
			cliproxyexecutor.RouteGroupMetadataKey: "smart",
		}}
		auth, _, err := manager.pickNext(context.Background(), "codex", "gpt-5.6-sol", opts, map[string]struct{}{})
		if err != nil {
			t.Fatalf("pickNext() error = %v", err)
		}
		return auth.ID
	}
	if got := pick(); got != "soon" {
		t.Fatalf("picked %q, want the account whose week renews first", got)
	}
	manager.SetSelector(&RoundRobinSelector{})
	for i := 0; i < 3; i++ {
		if got := pick(); got != "soon" {
			t.Fatalf("after a selector reload picked %q, want reset-aware ordering to stay", got)
		}
	}
}

// modelQuotaStub reports a load for one model only, the way an account whose
// Opus week is spent still has room for Sonnet.
type modelQuotaStub struct {
	model string
	ratio float64
}

func (s modelQuotaStub) QuotaLoadRatio(*Auth) (float64, bool) { return 0, true }

func (s modelQuotaStub) QuotaLoadRatioForModel(_ *Auth, model string) (float64, bool) {
	if model == s.model {
		return s.ratio, true
	}
	return 0, true
}

func TestLoadRatioAsksForTheRequestModel(t *testing.T) {
	deps := &schedulerDeps{
		tracker: newSelectionPressureTracker(),
		limiter: NewAccountConcurrencyLimiter(),
		quota:   modelQuotaStub{model: "claude-opus-4-5", ratio: 0.95},
	}
	auth := weightedAuth("a", 1)
	if got := deps.loadRatio(auth, "claude-opus-4-5(high)"); got != 0.95 {
		t.Fatalf("load for the spent model = %v, want 0.95", got)
	}
	if got := deps.loadRatio(auth, "claude-sonnet-4-5"); got != 0 {
		t.Fatalf("load for another model = %v, want 0", got)
	}
}

// A manager assembled without NewManager gets its selectors from SetSelector;
// the reset-aware one must still read the quota source.
func TestSetSelectorWiresTheResetAwareSelector(t *testing.T) {
	source := &stubWindowSource{windows: map[string][]QuotaWindow{}}
	manager := &Manager{}
	manager.SetQuotaLoadSource(source)
	manager.SetSelector(&RoundRobinSelector{})

	selector, ok := manager.distributionSelectorLocked("reset-aware").(*ResetAwareSelector)
	if !ok {
		t.Fatalf("reset-aware resolved to %T", manager.distributionSelectorLocked("reset-aware"))
	}
	if selector.deps.quotaWindows() != source {
		t.Fatalf("reset-aware selector does not read the quota source")
	}
}

package auth

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const simulationTestModel = "sim-model"

// simulationPoolGroup is a channel group holding every test credential; a
// request routed to it is scheduled by the distribution the test picks.
const simulationPoolGroup = "pool"

// newSimulationTestManager registers auths serving simulationTestModel (or the
// model in their "test_model" attribute) under one provider with an executor,
// and puts them all in simulationPoolGroup scheduled by distribution.
func newSimulationTestManager(t *testing.T, distribution string, auths ...*Auth) *Manager {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(&stubExecutor{id: "simprov"})
	reg := registry.GetGlobalRegistry()
	channels := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth.Provider == "" {
			auth.Provider = "simprov"
		}
		if auth.Status == "" {
			auth.Status = StatusActive
		}
		if auth.Label == "" {
			auth.Label = auth.ID
		}
		channels = append(channels, auth.Label)
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		model := simulationTestModel
		if other := auth.Attributes["test_model"]; other != "" {
			model = other
		}
		reg.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		id := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(id) })
	}
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{{
				Name:       simulationPoolGroup,
				Match:      internalconfig.ChannelGroupMatch{Channels: channels},
				Scheduling: internalconfig.GroupScheduling{Distribution: distribution},
			}},
		},
	})
	return manager
}

// poolMeta is the execution metadata of a request routed to the pool group.
func poolMeta() map[string]any {
	return map[string]any{cliproxyexecutor.RouteGroupMetadataKey: simulationPoolGroup}
}

// realFailoverOrder is the order successive real picks return, the way a
// request that fails on every credential walks them.
func realFailoverOrder(t *testing.T, manager *Manager, meta map[string]any) []string {
	t.Helper()
	tried := make(map[string]struct{})
	var order []string
	for range 50 {
		selected, _, _, err := manager.pickNextMixed(context.Background(), []string{"simprov"}, simulationTestModel, cliproxyexecutor.Options{Metadata: meta}, tried)
		if err != nil {
			break
		}
		tried[selected.ID] = struct{}{}
		order = append(order, selected.ID)
	}
	return order
}

func simulatedOrder(sim *SelectionSimulation) []string {
	order := make([]string, 0, len(sim.Attempts))
	for _, attempt := range sim.Attempts {
		order = append(order, attempt.AuthID)
	}
	return order
}

func cooledAuth(id string, until time.Time) *Auth {
	return &Auth{
		ID: id,
		ModelStates: map[string]*ModelState{simulationTestModel: {
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: until,
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			Quota:          QuotaState{Exceeded: true, NextRecoverAt: until},
		}},
	}
}

// The simulated order is the order real picks would take, from wherever live
// traffic left the round-robin cursors, and simulating does not move them.
func TestSimulateSelectionFollowsRealFailoverOrder(t *testing.T) {
	for _, distribution := range []string{"", "fill-first", "least-load", "reset-aware"} {
		t.Run("distribution="+distribution, func(t *testing.T) {
			manager := newSimulationTestManager(t, distribution,
				&Auth{ID: "sim-order-a-" + distribution, Attributes: map[string]string{"selection_weight": "3"}},
				&Auth{ID: "sim-order-b-" + distribution},
				&Auth{ID: "sim-order-c-" + distribution, Attributes: map[string]string{"selection_weight": "2"}},
				cooledAuth("sim-order-d-"+distribution, time.Now().Add(10*time.Minute)),
			)
			meta := poolMeta()
			// Live traffic before the simulation, so the cursors and the
			// selection pressure are not at their starting point.
			for range 4 {
				if _, _, _, err := manager.pickNextMixed(context.Background(), []string{"simprov"}, simulationTestModel, cliproxyexecutor.Options{Metadata: meta}, map[string]struct{}{}); err != nil {
					t.Fatalf("warm-up pick: %v", err)
				}
			}

			first := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, meta)
			second := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, meta)
			if !slices.Equal(simulatedOrder(first), simulatedOrder(second)) {
				t.Fatalf("simulations disagree: %v then %v; the first one moved live state", simulatedOrder(first), simulatedOrder(second))
			}
			real := realFailoverOrder(t, manager, meta)
			if got := simulatedOrder(first); !slices.Equal(got, real) {
				t.Fatalf("simulated order %v, real failover order %v", got, real)
			}
			if len(real) != 3 {
				t.Fatalf("real order %v: want the three credentials that are not cooling down", real)
			}
		})
	}
}

func TestSimulateSelectionLeavesLiveSchedulingStateAlone(t *testing.T) {
	manager := newSimulationTestManager(t, "",
		&Auth{ID: "sim-state-a"}, &Auth{ID: "sim-state-b"}, &Auth{ID: "sim-state-c"})
	meta := poolMeta()
	for range 2 {
		if _, _, _, err := manager.pickNextMixed(context.Background(), []string{"simprov"}, simulationTestModel, cliproxyexecutor.Options{Metadata: meta}, map[string]struct{}{}); err != nil {
			t.Fatalf("warm-up pick: %v", err)
		}
	}
	cursors, weighted, pressure := liveSchedulingState(manager)

	sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, meta)
	if len(sim.Attempts) != 3 {
		t.Fatalf("attempts = %v, want all three credentials", simulatedOrder(sim))
	}
	gotCursors, gotWeighted, gotPressure := liveSchedulingState(manager)
	if !reflect.DeepEqual(gotCursors, cursors) || !reflect.DeepEqual(gotWeighted, weighted) {
		t.Fatalf("round-robin state changed:\ncursors %v -> %v\nweighted %v -> %v", cursors, gotCursors, weighted, gotWeighted)
	}
	if !reflect.DeepEqual(gotPressure, pressure) {
		t.Fatalf("selection pressure changed: %v -> %v", pressure, gotPressure)
	}
}

func liveSchedulingState(m *Manager) (map[string]int, map[string]map[string]int, map[string]decayCounter) {
	rr := m.roundRobinSelector
	rr.mu.Lock()
	cursors := make(map[string]int, len(rr.cursors))
	for key, cursor := range rr.cursors {
		cursors[key] = cursor
	}
	weighted := make(map[string]map[string]int, len(rr.weighted))
	for key, state := range rr.weighted {
		current := make(map[string]int, len(state.current))
		for id, score := range state.current {
			current[id] = score
		}
		weighted[key] = current
	}
	rr.mu.Unlock()
	tracker := m.scheduler.tracker
	tracker.mu.Lock()
	pressure := make(map[string]decayCounter, len(tracker.counters))
	for id, counter := range tracker.counters {
		pressure[id] = counter
	}
	tracker.mu.Unlock()
	return cursors, weighted, pressure
}

// Every credential of the request's providers that the tenant may use is
// listed, with the gate that keeps it out or, for candidates, why it cannot
// take the request now.
func TestSimulateSelectionExplainsEachCredential(t *testing.T) {
	until := time.Now().Add(7 * time.Minute).Truncate(time.Second)
	manager := newSimulationTestManager(t, "",
		&Auth{ID: "sim-explain-ready", Label: "Ready"},
		cooledAuth("sim-explain-cooling", until),
		&Auth{ID: "sim-explain-disabled", Disabled: true, Status: StatusDisabled},
		&Auth{ID: "sim-explain-other-model", Attributes: map[string]string{"test_model": "another-model"}},
		&Auth{ID: "sim-explain-other-tenant", TenantID: "tenant-b"},
		&Auth{ID: "sim-explain-unweighted", Attributes: map[string]string{"selection_weight": "0"}},
	)
	sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, map[string]any{})
	if len(sim.Groups) != 1 {
		t.Fatalf("groups = %+v, want the one unscoped group", sim.Groups)
	}
	byID := map[string]CredentialSimulation{}
	for _, credential := range sim.Groups[0].Credentials {
		byID[credential.AuthID] = credential
	}
	if _, listed := byID["sim-explain-other-tenant"]; listed {
		t.Fatal("another tenant's credential is listed")
	}
	if got := byID["sim-explain-ready"]; got.Excluded != "" || got.Blocked != "" || got.Attempt != 1 || got.Label != "Ready" {
		t.Fatalf("ready credential = %+v, want a candidate tried first", got)
	}
	cooling := byID["sim-explain-cooling"]
	if cooling.Excluded != "" || cooling.Blocked != SimulatedBlockCooldown || cooling.Attempt != 0 {
		t.Fatalf("cooling credential = %+v, want a candidate blocked by cooldown and never tried", cooling)
	}
	if !cooling.BlockedUntil.Equal(until) || cooling.BlockCause != CooldownReasonQuotaExhausted || cooling.BlockHTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("cooling credential = %+v, want until %v, cause %s, status 429", cooling, until, CooldownReasonQuotaExhausted)
	}
	if got := byID["sim-explain-disabled"].Excluded; got != ExcludedDisabled {
		t.Fatalf("disabled credential excluded by %q, want %q", got, ExcludedDisabled)
	}
	if got := byID["sim-explain-other-model"]; got.Excluded != ExcludedModel || len(got.ModelExcludedBy)+len(got.ModelNotAllowedBy) != 0 {
		t.Fatalf("credential without the model = %+v, want excluded by model with no group to blame", got)
	}
	if got := byID["sim-explain-unweighted"]; got.Blocked != SimulatedBlockWeight || got.Attempt != 0 {
		t.Fatalf("weight-0 credential = %+v, want blocked by weight", got)
	}
	if got := simulatedOrder(sim); !slices.Equal(got, []string{"sim-explain-ready"}) {
		t.Fatalf("attempts = %v, want only the ready credential", got)
	}
}

// A channel group's model lists are named when they are what keeps a
// credential out.
func TestSimulateSelectionNamesTheGroupThatRefusesTheModel(t *testing.T) {
	manager := newSimulationTestManager(t, "", &Auth{ID: "sim-group-excluded", Label: "Team Channel"})
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{{
				Name:           "team",
				Match:          internalconfig.ChannelGroupMatch{Channels: []string{"Team Channel"}},
				ExcludedModels: []string{simulationTestModel},
			}},
		},
	})
	sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, map[string]any{
		cliproxyexecutor.RouteGroupMetadataKey: "team",
	})
	if len(sim.Groups) != 1 || sim.Groups[0].Name != "team" {
		t.Fatalf("groups = %+v, want the team group", sim.Groups)
	}
	credential := sim.Groups[0].Credentials[0]
	if credential.Excluded != ExcludedModel || !slices.Equal(credential.ModelExcludedBy, []string{"team"}) {
		t.Fatalf("credential = %+v, want excluded by the team group's excluded-models", credential)
	}
	if sim.Groups[0].Error == nil || sim.Error == nil {
		t.Fatalf("group error %+v, selection error %+v: want both to say no credential serves the model", sim.Groups[0].Error, sim.Error)
	}
}

// When every candidate is cooling down the simulation reports the error the
// client gets and how long a request waits before trying again.
func TestSimulateSelectionReportsCooldownErrorAndRetryWait(t *testing.T) {
	until := time.Now().Add(4 * time.Minute)
	manager := newSimulationTestManager(t, "",
		cooledAuth("sim-retry-a", until), cooledAuth("sim-retry-b", until.Add(time.Minute)))
	manager.SetRetryConfig(1, 10*time.Minute)

	sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, map[string]any{})
	if len(sim.Attempts) != 0 {
		t.Fatalf("attempts = %v, want none", simulatedOrder(sim))
	}
	if sim.Error == nil || sim.Error.Code != "model_cooldown" || sim.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("error = %+v, want model_cooldown 429", sim.Error)
	}
	if sim.Error.RetryAfter <= 3*time.Minute || sim.Error.RetryAfter > 4*time.Minute {
		t.Fatalf("retry-after = %v, want the soonest cooldown, about 4m", sim.Error.RetryAfter)
	}
	if sim.RetryWait <= 3*time.Minute || sim.RetryWait > 4*time.Minute || sim.RetryWaitKind != SimulatedRetryWaitRequestRetry {
		t.Fatalf("retry wait = %v (%q), want the soonest cooldown within max-retry-interval", sim.RetryWait, sim.RetryWaitKind)
	}

	manager.SetRetryConfig(1, time.Minute)
	if sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, map[string]any{}); sim.RetryWait != 0 || sim.RetryWaitKind != "" {
		t.Fatalf("retry wait = %v (%q) with a 1m max-retry-interval, want none", sim.RetryWait, sim.RetryWaitKind)
	}
}

// rateLimitedAuth is parked for a while after a 429 that spent no quota: the
// kind of cooldown a busy wait waits for.
func rateLimitedAuth(id string, now time.Time, until time.Time) *Auth {
	return &Auth{
		ID: id,
		ModelStates: map[string]*ModelState{simulationTestModel: {
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: until,
			UpdatedAt:      now,
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
		}},
	}
}

// A group that busy-waits makes a request whose every candidate is cooling
// wait for the soonest one instead of failing, and the simulation says so,
// without taking a waiter slot or a round of any request's budget.
func TestSimulateSelectionReportsTheGroupsBusyWait(t *testing.T) {
	now := time.Now()
	until := now.Add(40 * time.Second)
	manager := newSimulationTestManager(t, "",
		rateLimitedAuth("sim-busy-a", now, until), rateLimitedAuth("sim-busy-b", now, until.Add(time.Minute)))
	busyWait := func(seconds *int) {
		manager.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{{
				Name:               simulationPoolGroup,
				Match:              internalconfig.ChannelGroupMatch{Channels: []string{"sim-busy-a", "sim-busy-b"}},
				BusyWaitMaxSeconds: seconds,
			}},
		}})
	}

	cases := []struct {
		name     string
		seconds  *int
		wantKind string
	}{
		{"group waits a minute", new(60), SimulatedRetryWaitBusyWait},
		{"group does not wait", nil, ""},
		{"recovery beyond the group's budget", new(20), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busyWait(tc.seconds)
			sim := manager.SimulateSelection(context.Background(), []string{"simprov"}, simulationTestModel, poolMeta())
			if len(sim.Attempts) != 0 || sim.Error == nil {
				t.Fatalf("attempts %v, error %+v: want every candidate cooling", simulatedOrder(sim), sim.Error)
			}
			if sim.RetryWaitKind != tc.wantKind {
				t.Fatalf("retry wait kind = %q, want %q", sim.RetryWaitKind, tc.wantKind)
			}
			if tc.wantKind == "" && sim.RetryWait != 0 {
				t.Fatalf("retry wait = %v, want none", sim.RetryWait)
			}
			if tc.wantKind != "" && (sim.RetryWait <= 30*time.Second || sim.RetryWait > 40*time.Second) {
				t.Fatalf("retry wait = %v, want the time to the soonest recovery, about 40s", sim.RetryWait)
			}
			if waiting := manager.busyWaiters.Load(); waiting != 0 {
				t.Fatalf("%d waiter slots held after a simulation", waiting)
			}
		})
	}
}

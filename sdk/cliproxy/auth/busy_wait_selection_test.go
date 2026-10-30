package auth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestBusyWaitUsesScopedSelectionWeights(t *testing.T) {
	for _, scope := range []string{"route", "allowed", "default"} {
		t.Run(scope, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &scriptedExecutor{}
				mgr := newBusyWaitManager(t, exec)
				for _, id := range []string{"excluded-free", "eligible-cooling"} {
					if _, err := mgr.Register(context.Background(), &Auth{ID: id, Label: id, Provider: "flaky", Status: StatusActive}); err != nil {
						t.Fatal(err)
					}
				}
				group := "team"
				opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
				switch scope {
				case "route":
					opts.Metadata[cliproxyexecutor.RouteGroupMetadataKey] = group
				case "allowed":
					opts.Metadata["allowed-channel-groups"] = group
				case "default":
					group = "default"
				}
				mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
					IncludeDefaultGroup: true,
					ChannelGroups: []sdkconfig.RoutingChannelGroup{{
						Name:       group,
						Match:      sdkconfig.ChannelGroupMatch{Channels: []string{"excluded-free", "eligible-cooling"}},
						Scheduling: sdkconfig.GroupScheduling{ChannelWeights: map[string]int{"excluded-free": 0, "eligible-cooling": 1}},
					}},
				}})
				park(mgr, "eligible-cooling", upstreamDown, 15*time.Second)
				start := time.Now()
				if _, err := mgr.Execute(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, opts); err != nil {
					t.Fatalf("Execute: %v", err)
				}
				if waited := time.Since(start); waited < 15*time.Second || waited > 18*time.Second {
					t.Fatalf("waited %s, want eligible account recovery, ignoring healthy weight-0 account", waited)
				}
				if calls := exec.Calls(); len(calls) != 1 || calls[0] != "eligible-cooling" {
					t.Fatalf("calls = %v, want only eligible-cooling", calls)
				}
			})
		})
	}
}

func TestBusyWaitHonorsGroupWeightOverride(t *testing.T) {
	for _, test := range []struct {
		name        string
		rawWeight   string
		groupWeight int
		wantWait    bool
	}{
		{name: "group enables raw zero", rawWeight: "0", groupWeight: 1, wantWait: true},
		{name: "group excludes raw positive", rawWeight: "1", groupWeight: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &scriptedExecutor{}
				mgr := newBusyWaitManager(t, exec)
				if _, err := mgr.Register(context.Background(), &Auth{ID: "weighted", Label: "weighted", Provider: "flaky", Status: StatusActive,
					Attributes: map[string]string{selectionWeightAttribute: test.rawWeight}}); err != nil {
					t.Fatal(err)
				}
				mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
					IncludeDefaultGroup: true,
					ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", Scheduling: sdkconfig.GroupScheduling{
						ChannelWeights: map[string]int{"weighted": test.groupWeight},
					}}},
				}})
				park(mgr, "weighted", upstreamDown, 15*time.Second)
				start := time.Now()
				err := execute(mgr, context.Background())
				waited := time.Since(start)
				if test.wantWait {
					if err != nil || waited < 15*time.Second || waited > 18*time.Second {
						t.Fatalf("Execute = %v after %s, want successful recovery after 15-18s", err, waited)
					}
				} else if err == nil || waited != 0 || len(exec.Calls()) != 0 {
					t.Fatalf("Execute = %v after %s, calls = %v; excluded account must fail without waiting or execution", err, waited, exec.Calls())
				}
			})
		})
	}
}

func TestBusyWaitUsesSelectorsRouteFallbackBoundary(t *testing.T) {
	for _, test := range []struct {
		name             string
		primaryCooldown  time.Duration
		fallbackCooldown time.Duration
		primaryExcluded  bool
		wantWait         bool
		wantAuth         string
	}{
		{name: "healthy fallback does not suppress primary recovery", primaryCooldown: 15 * time.Second, wantWait: true, wantAuth: "primary"},
		{name: "cooling fallback cannot shorten primary recovery", primaryCooldown: 2 * time.Minute, fallbackCooldown: 15 * time.Second},
		{name: "weighted-out primary does not fall back", primaryCooldown: 15 * time.Second, fallbackCooldown: 15 * time.Second, primaryExcluded: true},
		{name: "empty primary falls back", fallbackCooldown: 15 * time.Second, wantWait: true, wantAuth: "fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				exec := &scriptedExecutor{}
				mgr := newBusyWaitManager(t, exec, "fallback")
				mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{IncludeDefaultGroup: true}})
				if test.primaryExcluded {
					mgr.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
						IncludeDefaultGroup: true,
						ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "pro", Scheduling: sdkconfig.GroupScheduling{
							ChannelWeights: map[string]int{"flaky": 0},
						}}},
					}})
				}
				if test.primaryCooldown > 0 {
					if _, err := mgr.Register(context.Background(), &Auth{ID: "primary", Provider: "flaky", Prefix: "pro", Status: StatusActive}); err != nil {
						t.Fatal(err)
					}
					park(mgr, "primary", upstreamDown, test.primaryCooldown)
				}
				if test.fallbackCooldown > 0 {
					park(mgr, "fallback", upstreamDown, test.fallbackCooldown)
				}
				opts := cliproxyexecutor.Options{Metadata: map[string]any{
					cliproxyexecutor.RouteGroupMetadataKey:    "pro",
					cliproxyexecutor.RouteFallbackMetadataKey: "default",
				}}
				start := time.Now()
				_, err := mgr.Execute(context.Background(), []string{"flaky"}, cliproxyexecutor.Request{Model: "test-model"}, opts)
				waited := time.Since(start)
				if test.wantWait {
					if err != nil || waited < 15*time.Second || waited > 18*time.Second {
						t.Fatalf("Execute = %v after %s, want successful recovery after 15-18s", err, waited)
					}
					if calls := exec.Calls(); len(calls) != 1 || calls[0] != test.wantAuth {
						t.Fatalf("calls = %v, want only %s", calls, test.wantAuth)
					}
				} else if err == nil || waited != 0 || len(exec.Calls()) != 0 {
					t.Fatalf("Execute = %v after %s, calls = %v; unreachable fallback must not cause waiting", err, waited, exec.Calls())
				}
			})
		})
	}
}

func TestBusyWaitUsesAlphaSearchCapabilityFilter(t *testing.T) {
	for _, test := range []struct {
		name     string
		eligible bool
		fallback bool
	}{
		{name: "no eligible account"},
		{name: "healthy ineligible account does not suppress recovery", eligible: true},
		{name: "ineligible primary permits eligible fallback", eligible: true, fallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ineligible := &Auth{ID: "generic", Provider: "codex", Status: StatusActive,
					Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.invalid/v1"}}
				candidates := []*Auth{ineligible}
				coolingID := ineligible.ID
				if test.eligible {
					candidates = append(candidates, alphaSearchTestOAuth("search"))
					coolingID = "search"
				}
				if test.fallback {
					ineligible.Prefix = "pro"
				}
				mgr := alphaSearchTestManager(t, candidates...)
				retryAfter := 15 * time.Second
				mgr.MarkResult(context.Background(), Result{AuthID: coolingID, Provider: "codex", Model: alphaSearchTestModel, Error: upstreamDown, RetryAfter: &retryAfter})
				opts := cliproxyexecutor.Options{Alt: "alpha/search"}
				if test.fallback {
					opts.Metadata = map[string]any{
						cliproxyexecutor.RouteGroupMetadataKey:    "pro",
						cliproxyexecutor.RouteFallbackMetadataKey: "default",
					}
				}
				_, _, _, err := mgr.pickNextMixed(context.Background(), []string{"codex"}, alphaSearchTestModel, opts, nil)
				if err == nil {
					t.Fatal("selector must not pick a healthy account that lacks search capability")
				}
				wait, ok := mgr.busyWaitFor(context.Background(), err, []string{"codex"}, alphaSearchTestModel, opts, &retryWaitBudget{})
				if ok != test.eligible || (ok && (wait < 15*time.Second || wait > 18*time.Second)) {
					t.Fatalf("busyWaitFor = (%s, %v), want wait=%v for only search-capable accounts", wait, ok, test.eligible)
				}
			})
		})
	}
}

func TestBusyWaitUsesConcurrencyFilteredCandidates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &scriptedExecutor{}
		mgr := newBusyWaitManager(t, exec, "cooling")
		saturated := &Auth{ID: "saturated", Provider: "flaky", Status: StatusActive, Attributes: map[string]string{"concurrency_limit": "1"}}
		if _, err := mgr.Register(context.Background(), saturated); err != nil {
			t.Fatal(err)
		}
		release, err := mgr.concurrencyLimiter.AcquireSlot(saturated)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		park(mgr, "cooling", upstreamDown, 15*time.Second)
		start := time.Now()
		if err := execute(mgr, context.Background()); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if waited := time.Since(start); waited < 15*time.Second || waited > 18*time.Second {
			t.Fatalf("waited %s, want recovery of the concurrency-eligible account", waited)
		}
		if calls := exec.Calls(); len(calls) != 1 || calls[0] != "cooling" {
			t.Fatalf("calls = %v, want only cooling", calls)
		}
	})
}

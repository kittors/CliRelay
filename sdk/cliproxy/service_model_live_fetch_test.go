package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
)

// listedOnlyModel stands for a model an upstream lists and no compiled-in list
// has. Seeing it registered is how these tests tell a live or saved list from the
// floor.
const listedOnlyModel = "gemini-listed-upstream-only"

func antigravityOAuthAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:         id,
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
	}
}

func listedModels(provider string, ids ...string) []*ModelInfo {
	out := make([]*ModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, &ModelInfo{ID: id, Object: "model", OwnedBy: provider, Type: provider, DisplayName: id})
	}
	return out
}

// floorOnlyModel names a model of a provider's compiled-in floor that nothing
// else registers, so its presence means the credential is serving the floor.
func floorOnlyModel(t *testing.T, provider string) string {
	t.Helper()
	always := make(map[string]struct{})
	for _, model := range serviceapp.WithRegisteredImageModels(provider, nil) {
		always[strings.ToLower(model.ID)] = struct{}{}
	}
	for _, model := range serviceapp.CredentialModelFloor(provider) {
		if _, added := always[strings.ToLower(model.ID)]; !added && model.ID != listedOnlyModel {
			return model.ID
		}
	}
	t.Fatalf("the %s floor has no model of its own", provider)
	return ""
}

func stubWatcherFactory(string, string, func(*config.Config)) (*WatcherWrapper, error) {
	return &WatcherWrapper{
		start:                 func(context.Context) error { return nil },
		stop:                  func() error { return nil },
		setConfig:             func(*config.Config) {},
		setUpdateQueue:        func(chan<- runtimeAuthUpdate) {},
		dispatchRuntimeUpdate: func(runtimeAuthUpdate) bool { return false },
	}, nil
}

// selfListingService is a service whose model listing is fetch and whose saved
// lists live at snapshot. Cleanup stops its background work before unregistering
// its credentials, so a late fetch cannot register them again.
func selfListingService(t *testing.T, manager *coreauth.Manager, snapshot string, fetch liveModelFetchFunc) *Service {
	t.Helper()
	resetModelDiscovery(t)
	service := &Service{
		cfg:            &config.Config{AuthDir: t.TempDir()},
		configPath:     filepath.Join(t.TempDir(), "config.yaml"),
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		watcherFactory: stubWatcherFactory,
		coreManager:    manager,
	}
	service.modelLists.setPath(snapshot)
	service.liveModels.fetch = fetch
	t.Cleanup(func() {
		for _, auth := range manager.List() {
			GlobalModelRegistry().UnregisterClient(auth.ID)
		}
	})
	t.Cleanup(service.stopProviderDiscovery)
	t.Cleanup(service.stopLiveModelLists)
	return service
}

// storedAuthsManager is a manager whose store holds auths, which is where
// loadInitialState reads credentials from.
func storedAuthsManager(auths ...*coreauth.Auth) *coreauth.Manager {
	for _, auth := range auths {
		GlobalModelRegistry().UnregisterClient(auth.ID)
	}
	return coreauth.NewManager(&startupStoreStub{auths: auths}, &coreauth.RoundRobinSelector{}, nil)
}

// liveModelFailures reports a credential's consecutive failed listings.
func (s *Service) liveModelFailures(authID string) int {
	st := &s.liveModels
	st.mu.Lock()
	defer st.mu.Unlock()
	if status := st.status[authID]; status != nil {
		return status.failures
	}
	return 0
}

// testClock is a clock tests move by hand; the fetcher reads it concurrently.
type testClock struct{ nanos atomic.Int64 }

func newTestClock() *testClock {
	clock := &testClock{}
	clock.nanos.Store(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC).UnixNano())
	return clock
}

func (c *testClock) Now() time.Time           { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *testClock) Advance(by time.Duration) { c.nanos.Add(int64(by)) }

// TestRunServesWhileModelListingsHang is the 2026-09-25 outage: a node whose
// egress cannot reach the credentials' proxies, where every listing call hangs.
// The process used to list each credential in turn before listening, and took
// longer to start than the deploy's readiness window allowed.
func TestRunServesWhileModelListingsHang(t *testing.T) {
	var started, running, peak atomic.Int32
	hang := func(ctx context.Context, _ *coreauth.Auth, _ *config.Config, _ string) ([]*ModelInfo, error) {
		started.Add(1)
		now := running.Add(1)
		defer running.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	const credentials = 3 * liveModelFetchConcurrency
	ids := make([]string, 0, credentials)
	auths := make([]*coreauth.Auth, 0, credentials)
	for i := range credentials {
		auth := antigravityOAuthAuth(fmt.Sprintf("ag-hanging-listing-%02d", i))
		auths = append(auths, auth)
		ids = append(ids, auth.ID)
	}
	manager := storedAuthsManager(auths...)
	service := selfListingService(t, manager, filepath.Join(t.TempDir(), "lists.json"), hang)
	// Only shutdown ends a listing here: startup cannot be rescued by a timeout.
	service.liveModels.timeout = time.Hour
	listening := make(chan struct{})
	service.hooks.OnAfterStart = func(*Service) { close(listening) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	began := time.Now()
	go func() { runErr <- service.Run(ctx) }()

	select {
	case <-listening:
	case err := <-runErr:
		t.Fatalf("Run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the API server did not start while every model listing hangs")
	}
	t.Logf("listening %s after Run with every model listing hanging", time.Since(began).Round(time.Millisecond))

	reg := GlobalModelRegistry()
	for _, id := range ids {
		if len(reg.GetModelsForClient(id)) == 0 {
			t.Fatalf("%s serves no models while its listing hangs", id)
		}
	}

	// The listings do run, in the background, no more of them at once than the bound.
	waitForCondition(t, "the listings to start", func() bool { return started.Load() >= liveModelFetchConcurrency })
	time.Sleep(50 * time.Millisecond)
	if got := started.Load(); got != liveModelFetchConcurrency {
		t.Fatalf("listings started = %d, want the %d the bound allows while they hang", got, liveModelFetchConcurrency)
	}
	if got := peak.Load(); got > liveModelFetchConcurrency {
		t.Fatalf("listings in flight peaked at %d, bound is %d", got, liveModelFetchConcurrency)
	}

	// Shutdown cancels them instead of waiting them out.
	cancel()
	select {
	case <-runErr:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown waited on the hanging model listings")
	}
	if got := running.Load(); got != 0 {
		t.Fatalf("%d model listings still running after shutdown", got)
	}
}

func TestLiveModelListReplacesTheFloorWhenItArrives(t *testing.T) {
	release := make(chan struct{})
	fetch := func(ctx context.Context, _ *coreauth.Auth, _ *config.Config, provider string) ([]*ModelInfo, error) {
		select {
		case <-release:
			return listedModels(provider, listedOnlyModel), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	auth := antigravityOAuthAuth("ag-live-list-arrives")
	manager := storedAuthsManager(auth)
	snapshot := filepath.Join(t.TempDir(), "lists.json")
	service := selfListingService(t, manager, snapshot, fetch)
	floorOnly := floorOnlyModel(t, "antigravity")

	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	reg := GlobalModelRegistry()
	if !reg.ClientSupportsModel(auth.ID, floorOnly) {
		t.Fatalf("with nothing ever listed the credential serves the floor; got %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
	if reg.ClientSupportsModel(auth.ID, listedOnlyModel) {
		t.Fatal("registered a model before anything listed it")
	}

	close(release)
	waitForCondition(t, "the live list to be registered", func() bool {
		return reg.ClientSupportsModel(auth.ID, listedOnlyModel)
	})
	// The live list replaces the floor rather than being merged into it.
	if reg.ClientSupportsModel(auth.ID, floorOnly) {
		t.Fatalf("floor model %s still registered next to the live list: %v", floorOnly, modelIDs(reg.GetModelsForClient(auth.ID)))
	}

	// It is saved for the next start.
	restored := &credentialModelLists{path: snapshot}
	if loaded := restored.load(); loaded != 1 {
		t.Fatalf("saved lists loaded = %d, want 1", loaded)
	}
	if models, source := restored.lookup(auth.ID, "antigravity"); source != modelListSourceOwn || !hasModelID(models, listedOnlyModel) {
		t.Fatalf("saved list = %v (%s), want the live list", modelIDs(models), source)
	}
}

// TestStartupServesTheSavedListWhileTheUpstreamIsDown is the restart the outage
// needed: the upstream answered before, is unreachable now, and the credential
// must serve what it listed last time rather than nothing or the floor.
func TestStartupServesTheSavedListWhileTheUpstreamIsDown(t *testing.T) {
	snapshot := filepath.Join(t.TempDir(), "upstream-model-lists.json")
	auth := antigravityOAuthAuth("ag-saved-list")
	manager := storedAuthsManager(auth)
	floorOnly := floorOnlyModel(t, "antigravity")
	reg := GlobalModelRegistry()

	previous := selfListingService(t, manager, snapshot, func(_ context.Context, _ *coreauth.Auth, _ *config.Config, provider string) ([]*ModelInfo, error) {
		return listedModels(provider, listedOnlyModel, "gemini-3.1-pro-high"), nil
	})
	if err := previous.loadInitialState(context.Background()); err != nil {
		t.Fatalf("previous loadInitialState: %v", err)
	}
	waitForCondition(t, "the previous run to register the live list", func() bool {
		return reg.ClientSupportsModel(auth.ID, listedOnlyModel)
	})
	previous.stopLiveModelLists()
	previous.stopProviderDiscovery()
	reg.UnregisterClient(auth.ID)

	var calls atomic.Int32
	next := selfListingService(t, manager, snapshot, func(context.Context, *coreauth.Auth, *config.Config, string) ([]*ModelInfo, error) {
		calls.Add(1)
		return nil, errors.New("socks connect tcp: dial tcp4: i/o timeout")
	})
	if err := next.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	// Straight away, before any listing has been tried.
	if !reg.ClientSupportsModel(auth.ID, listedOnlyModel) {
		t.Fatalf("the saved list is not registered at startup: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}

	// And after the listing failed.
	waitForCondition(t, "the listing to fail", func() bool { return next.liveModelFailures(auth.ID) == 1 })
	if !reg.ClientSupportsModel(auth.ID, listedOnlyModel) || !reg.ClientSupportsModel(auth.ID, "gemini-3.1-pro-high") {
		t.Fatalf("a failed listing dropped the saved list: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
	if reg.ClientSupportsModel(auth.ID, floorOnly) {
		t.Fatalf("a failed listing fell back to the floor: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
	if calls.Load() == 0 {
		t.Fatal("the listing was never attempted")
	}
}

func TestFailedModelListingKeepsTheLastKnownList(t *testing.T) {
	var failing atomic.Bool
	var calls atomic.Int32
	fetch := func(_ context.Context, _ *coreauth.Auth, _ *config.Config, provider string) ([]*ModelInfo, error) {
		calls.Add(1)
		if failing.Load() {
			return nil, errors.New("upstream answered status 503")
		}
		return listedModels(provider, listedOnlyModel), nil
	}
	auth := antigravityOAuthAuth("ag-listing-fails-later")
	manager := storedAuthsManager(auth)
	service := selfListingService(t, manager, filepath.Join(t.TempDir(), "lists.json"), fetch)
	clock := newTestClock()
	service.liveModels.now = clock.Now

	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	reg := GlobalModelRegistry()
	waitForCondition(t, "the live list to be registered", func() bool {
		return reg.ClientSupportsModel(auth.ID, listedOnlyModel)
	})

	// Later the upstream stops answering, and a re-registration (a token refresh,
	// say) asks for the list again once the last answer is no longer fresh.
	failing.Store(true)
	clock.Advance(liveModelFetchFreshFor + time.Second)
	before := calls.Load()
	current, _ := manager.GetByID(auth.ID)
	service.registerModelsForAuth(context.Background(), current)
	waitForCondition(t, "the failed listing to be recorded", func() bool { return service.liveModelFailures(auth.ID) == 1 })
	if calls.Load() <= before {
		t.Fatal("the stale list was not listed again")
	}
	if models := reg.GetModelsForClient(auth.ID); !hasModelID(models, listedOnlyModel) {
		t.Fatalf("a failed listing dropped the last known list: %v", modelIDs(models))
	}
}

func TestFailedModelListingIsRetriedAfterItsBackoff(t *testing.T) {
	var calls atomic.Int32
	fetch := func(_ context.Context, _ *coreauth.Auth, _ *config.Config, provider string) ([]*ModelInfo, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("upstream unreachable")
		}
		return listedModels(provider, listedOnlyModel), nil
	}
	auth := antigravityOAuthAuth("ag-listing-retried")
	manager := storedAuthsManager(auth)
	service := selfListingService(t, manager, filepath.Join(t.TempDir(), "lists.json"), fetch)
	clock := newTestClock()
	service.liveModels.now = clock.Now
	service.liveModels.sweepEvery = 5 * time.Millisecond

	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	waitForCondition(t, "the first listing to fail", func() bool { return service.liveModelFailures(auth.ID) == 1 })

	// Registrations inside the backoff do not ask again.
	current, _ := manager.GetByID(auth.ID)
	service.registerModelsForAuth(context.Background(), current)
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("listings = %d before the backoff was over, want 1", got)
	}

	clock.Advance(liveModelFetchRetryMin)
	reg := GlobalModelRegistry()
	waitForCondition(t, "the retry to register the live list", func() bool {
		return reg.ClientSupportsModel(auth.ID, listedOnlyModel)
	})
	if got := service.liveModelFailures(auth.ID); got != 0 {
		t.Fatalf("failures after a successful retry = %d, want 0", got)
	}
}

func TestModelListingCallsAreTimeBounded(t *testing.T) {
	const timeout = 50 * time.Millisecond
	deadlines := make(chan time.Duration, 1)
	fetch := func(ctx context.Context, _ *coreauth.Auth, _ *config.Config, _ string) ([]*ModelInfo, error) {
		left := time.Duration(-1)
		if deadline, ok := ctx.Deadline(); ok {
			left = time.Until(deadline)
		}
		select {
		case deadlines <- left:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	auth := antigravityOAuthAuth("ag-listing-times-out")
	manager := storedAuthsManager(auth)
	service := selfListingService(t, manager, filepath.Join(t.TempDir(), "lists.json"), fetch)
	service.liveModels.timeout = timeout

	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	select {
	case left := <-deadlines:
		if left < 0 || left > timeout {
			t.Fatalf("listing deadline %s away, want at most %s", left, timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listing never started")
	}
	// Cut off at the deadline and counted as a failed attempt, so it is retried.
	waitForCondition(t, "the timed-out listing to count as a failure", func() bool { return service.liveModelFailures(auth.ID) == 1 })
}

func TestSelfListingCredentialsRegisterTheFloorWithoutAnyList(t *testing.T) {
	for _, provider := range []string{"antigravity", "xai", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			service := &Service{cfg: &config.Config{}}
			service.liveModels.fetch = func(context.Context, *coreauth.Auth, *config.Config, string) ([]*ModelInfo, error) {
				calls.Add(1)
				return nil, errors.New("registration must not list inline")
			}
			auth := &coreauth.Auth{
				ID:         "self-listing-floor-" + provider,
				Provider:   provider,
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"auth_kind": "oauth"},
			}
			reg := unregisterOnCleanup(t, auth.ID)

			service.registerModelsForAuth(context.Background(), auth)

			if got := calls.Load(); got != 0 {
				t.Fatalf("registration listed models inline %d times", got)
			}
			registered := reg.GetModelsForClient(auth.ID)
			floor := serviceapp.CredentialModelFloor(provider)
			if len(floor) == 0 || len(registered) == 0 {
				t.Fatalf("floor = %d models, registered = %d; an unlisted credential must still serve something", len(floor), len(registered))
			}
			for _, model := range floor {
				if !hasModelID(registered, model.ID) {
					t.Fatalf("floor model %s missing from %v", model.ID, modelIDs(registered))
				}
			}
		})
	}
}

func TestAntigravityFloorLeavesOutEditorCompletionModels(t *testing.T) {
	for _, model := range serviceapp.CredentialModelFloor("antigravity") {
		id := strings.ToLower(model.ID)
		if strings.HasPrefix(id, "tab_") || strings.HasPrefix(id, "chat_") {
			t.Fatalf("antigravity floor includes the internal model %s", model.ID)
		}
	}
}

func TestRemovedCredentialForgetsItsList(t *testing.T) {
	fetch := func(_ context.Context, auth *coreauth.Auth, _ *config.Config, provider string) ([]*ModelInfo, error) {
		return listedModels(provider, listedOnlyModel, "model-of-"+auth.ID), nil
	}
	kept, removed := antigravityOAuthAuth("ag-list-kept"), antigravityOAuthAuth("ag-list-removed")
	manager := storedAuthsManager(kept, removed)
	service := selfListingService(t, manager, filepath.Join(t.TempDir(), "lists.json"), fetch)
	if err := service.loadInitialState(context.Background()); err != nil {
		t.Fatalf("loadInitialState: %v", err)
	}
	reg := GlobalModelRegistry()
	waitForCondition(t, "both lists to be registered", func() bool {
		return reg.ClientSupportsModel(kept.ID, "model-of-"+kept.ID) && reg.ClientSupportsModel(removed.ID, "model-of-"+removed.ID)
	})

	service.applyCoreAuthRemoval(context.Background(), removed.ID)

	if models, source := service.modelLists.lookup(removed.ID, "antigravity"); source == modelListSourceOwn {
		t.Fatalf("removed credential still has its own list: %v", modelIDs(models))
	}
	service.saveModelLists()
	restored := &credentialModelLists{path: service.modelLists.snapshotPath()}
	restored.load()
	if _, source := restored.lookup(removed.ID, "antigravity"); source == modelListSourceOwn {
		t.Fatal("removed credential's list was saved")
	}
	if _, source := restored.lookup(kept.ID, "antigravity"); source != modelListSourceOwn {
		t.Fatal("remaining credential's list was not saved")
	}
}

func TestLiveModelListingBackoffDoublesUpToItsCap(t *testing.T) {
	st := &liveModelFetcher{}
	st.applyDefaults()
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, expected := range want {
		if got := st.backoff(i + 1); got != expected {
			t.Fatalf("backoff after %d failures = %s, want %s", i+1, got, expected)
		}
	}
}

func TestUnreadableSavedListsAreIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lists.json")
	for name, content := range map[string]string{
		"torn":            `{"version":1,"lists":{"a":`,
		"unknown version": `{"version":99,"lists":{"a":{"provider":"antigravity","models":[{"id":"x"}]}}}`,
		"other provider":  `{"version":1,"lists":{"a":{"provider":"codex","models":[{"id":"x"}]}}}`,
		"no models":       `{"version":1,"lists":{"a":{"provider":"antigravity","models":[{"id":" "}]}}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		lists := &credentialModelLists{path: path}
		if loaded := lists.load(); loaded != 0 {
			t.Fatalf("%s: loaded %d lists, want none", name, loaded)
		}
	}
	missing := &credentialModelLists{path: filepath.Join(t.TempDir(), "absent.json")}
	if loaded := missing.load(); loaded != 0 {
		t.Fatalf("missing file loaded %d lists", loaded)
	}
}

package contentmoderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

type runtimeEvaluatorStub struct {
	calls    atomic.Int32
	decision Decision
}

func (e *runtimeEvaluatorStub) Evaluate(context.Context, Profile, Input) Decision {
	e.calls.Add(1)
	return e.decision
}

type blockingRuntimeEvaluator struct {
	started  chan struct{}
	release  chan struct{}
	decision Decision
}

func (e *blockingRuntimeEvaluator) Evaluate(context.Context, Profile, Input) Decision {
	close(e.started)
	<-e.release
	return e.decision
}

type runtimeResolverStub struct {
	calls atomic.Int32
	fn    func() (Profile, string, error)
}

func (r *runtimeResolverStub) ResolveProfile(context.Context, string, string, string, string) (Profile, string, error) {
	r.calls.Add(1)
	return r.fn()
}

type runtimeOrderedSelector struct{}

func (runtimeOrderedSelector) Pick(_ context.Context, _ string, _ string, _ cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	if len(auths) == 0 {
		return nil, errors.New("no auth")
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths[0], nil
}

type runtimeExecutor struct {
	mu         sync.Mutex
	calls      map[string]int
	failAuthID string
}

func (e *runtimeExecutor) Identifier() string { return "moderation-runtime" }

func (e *runtimeExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.calls[auth.ID]++
	e.mu.Unlock()
	if auth.ID == e.failAuthID {
		return cliproxyexecutor.Response{}, errors.New("upstream unavailable")
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *runtimeExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	chunks := make(chan cliproxyexecutor.StreamChunk)
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *runtimeExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *runtimeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *runtimeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func createRuntimeProfile(t *testing.T, store *Store, tenantID, id string, input CreateProfileInput) Profile {
	t.Helper()
	profile, err := NewProfile(tenantID, id, input, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	if err = store.CreateProfile(context.Background(), profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return profile
}

func bindRuntimeChannel(t *testing.T, store *Store, tenantID, channelType, channelID, profileID string) {
	t.Helper()
	if err := store.PatchBindings(context.Background(), tenantID, false, []BindingOperation{{
		ChannelType: channelType,
		ChannelID:   channelID,
		ProfileID:   &profileID,
	}}); err != nil {
		t.Fatalf("PatchBindings: %v", err)
	}
}

func runtimeOptions(tenantID, text string) cliproxyexecutor.Options {
	body, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": text}}})
	return cliproxyexecutor.Options{
		OriginalRequest: body,
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	}
}

func newRuntimeManager(t *testing.T, moderator coreauth.RequestModerator, executor *runtimeExecutor, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, runtimeOrderedSelector{}, nil)
	manager.SetRequestModerator(moderator)
	registeredTenants := make(map[string]struct{})
	for _, auth := range auths {
		tenantID := coreauth.NormalizedTenantID(auth.TenantID)
		if _, exists := registeredTenants[tenantID]; !exists {
			manager.RegisterExecutorForTenant(tenantID, executor)
			registeredTenants[tenantID] = struct{}{}
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func TestRuntimeModeratorAuthOffOverridesProviderBinding(t *testing.T) {
	const tenantID = "tenant-auth-off"
	store := newTestStore(t)
	authProfile := createRuntimeProfile(t, store, tenantID, "profile-auth-off", CreateProfileInput{Name: "auth off", Mode: ModeOff})
	providerProfile := createRuntimeProfile(t, store, tenantID, "profile-provider-block", CreateProfileInput{
		Name:            "provider block",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"blocked"},
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeAuthFile, "auth-file", authProfile.ID)
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProvider, "provider-id", providerProfile.ID)
	evaluator := &runtimeEvaluatorStub{decision: Decision{WouldBlock: true, Action: ActionKeywordBlock}}
	moderator := NewRequestModerator(store, evaluator)
	auth := &coreauth.Auth{
		ID:       "auth-file",
		TenantID: tenantID,
		FileName: "auth-file.json",
		Provider: "moderation-runtime",
		Attributes: map[string]string{
			"provider_config_id": "provider-id",
		},
	}

	result := moderator.Moderate(context.Background(), auth, runtimeOptions(tenantID, "blocked"))
	if result.Blocked {
		t.Fatal("auth-specific off profile must allow without provider fallback")
	}
	if evaluator.calls.Load() != 0 {
		t.Fatalf("evaluator calls = %d, want zero", evaluator.calls.Load())
	}
}

func TestRuntimeModeratorBlocksOpenAIImagePrompt(t *testing.T) {
	const tenantID = "tenant-image-prompt"
	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "profile-image-prompt", CreateProfileInput{
		Name:            "image prompt block",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"bad word"},
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "image-key", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(nil))
	auth := &coreauth.Auth{
		ID:       "image-auth",
		TenantID: tenantID,
		Provider: "moderation-runtime",
		Attributes: map[string]string{
			"provider_key_id": "image-key",
		},
	}
	opts := cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"model":"gpt-image-2","prompt":"bad word"}`),
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	}

	if result := moderator.Moderate(context.Background(), auth, opts); !result.Blocked {
		t.Fatal("images-style prompt was not blocked")
	}
}

func TestRuntimeModeratorMetricsAreTenantScoped(t *testing.T) {
	profileA, err := NewProfile("tenant-a", "profile-a", CreateProfileInput{
		Name: "a", Mode: ModePreBlock, KeywordMode: KeywordModeKeywordOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile A: %v", err)
	}
	profileB, err := NewProfile("tenant-b", "profile-b", CreateProfileInput{
		Name: "b", Mode: ModePreBlock, KeywordMode: KeywordModeKeywordOnly,
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile B: %v", err)
	}
	moderator := NewRequestModerator(tenantMetricsResolver{profiles: map[string]Profile{
		"tenant-a": profileA,
		"tenant-b": profileB,
	}}, &runtimeEvaluatorStub{decision: Decision{Action: ActionAllow}})

	for i := 0; i < 2; i++ {
		moderator.Moderate(context.Background(), &coreauth.Auth{
			ID: "a", TenantID: "tenant-a", Attributes: map[string]string{"provider_key_id": "k-a"},
		}, runtimeOptions("tenant-a", "safe"))
	}
	moderator.Moderate(context.Background(), &coreauth.Auth{
		ID: "b", TenantID: "tenant-b", Attributes: map[string]string{"provider_key_id": "k-b"},
	}, runtimeOptions("tenant-b", "safe"))

	if got := moderator.MetricsForTenant("tenant-a"); got.Requests != 2 || got.Allows != 2 {
		t.Fatalf("tenant-a metrics = %#v", got)
	}
	if got := moderator.MetricsForTenant("tenant-b"); got.Requests != 1 || got.Allows != 1 {
		t.Fatalf("tenant-b metrics = %#v", got)
	}
	if got := moderator.MetricsForTenant("tenant-missing"); got.Requests != 0 {
		t.Fatalf("missing tenant metrics = %#v", got)
	}
}

type tenantMetricsResolver struct {
	profiles map[string]Profile
}

func (r tenantMetricsResolver) ResolveProfile(_ context.Context, tenantID, _, _, _ string) (Profile, string, error) {
	profile, ok := r.profiles[tenantID]
	if !ok {
		return Profile{}, "", ErrNotFound
	}
	return profile, ChannelTypeProviderKey, nil
}

func TestRuntimeModeratorMetricsTrackAPILatencyAndInFlight(t *testing.T) {
	const tenantID = "tenant-metrics"
	profile, err := NewProfile(tenantID, "profile-metrics", CreateProfileInput{
		Name:        "metrics",
		Mode:        ModePreBlock,
		KeywordMode: KeywordModeAPIOnly,
		APIKey:      "moderation-secret",
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	resolver := &runtimeResolverStub{fn: func() (Profile, string, error) {
		return profile, ChannelTypeProviderKey, nil
	}}
	evaluator := &blockingRuntimeEvaluator{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		decision: Decision{Action: ActionAllow, LatencyMS: 24},
	}
	moderator := NewRequestModerator(resolver, evaluator)
	auth := &coreauth.Auth{
		ID:       "auth-metrics",
		TenantID: tenantID,
		Attributes: map[string]string{
			"provider_key_id": "key-metrics",
		},
	}
	resultCh := make(chan coreauth.RequestModerationResult, 1)
	go func() {
		resultCh <- moderator.Moderate(context.Background(), auth, runtimeOptions(tenantID, "safe"))
	}()

	select {
	case <-evaluator.started:
	case <-time.After(time.Second):
		t.Fatal("moderation evaluator did not start")
	}
	if metrics := moderator.Metrics(); metrics.Requests != 1 || metrics.InFlight != 1 {
		t.Fatalf("metrics during evaluation = %#v", metrics)
	}
	close(evaluator.release)
	if result := <-resultCh; result.Blocked {
		t.Fatal("allow decision unexpectedly blocked")
	}
	metrics := moderator.Metrics()
	if metrics.Requests != 1 || metrics.Allows != 1 || metrics.Blocks != 0 || metrics.Errors != 0 || metrics.InFlight != 0 {
		t.Fatalf("metrics after evaluation = %#v", metrics)
	}
	if metrics.LatencyTotalMS != 24 || metrics.LatencySamples != 1 || metrics.AvgLatencyMS != 24 {
		t.Fatalf("latency metrics = %#v", metrics)
	}
}

func TestRuntimeModeratorModerationAPIFailureCallsExecutor(t *testing.T) {
	const tenantID = "tenant-api-error"
	var moderationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		moderationCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "profile-api", CreateProfileInput{
		Name:        "api",
		Mode:        ModePreBlock,
		KeywordMode: KeywordModeAPIOnly,
		APIKey:      "moderation-secret",
		BaseURL:     server.URL,
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-a", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(server.Client()))
	executor := &runtimeExecutor{calls: make(map[string]int)}
	manager := newRuntimeManager(t, moderator, executor, &coreauth.Auth{
		ID:       "auth-a",
		TenantID: tenantID,
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"provider_key_id": "key-a",
		},
	})

	if _, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, runtimeOptions(tenantID, "hello")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if moderationCalls.Load() != 1 {
		t.Fatalf("moderation calls = %d, want 1", moderationCalls.Load())
	}
	executor.mu.Lock()
	calls := executor.calls["auth-a"]
	executor.mu.Unlock()
	if calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
	if metrics := moderator.Metrics(); metrics.Errors != 1 || metrics.Allows != 1 {
		t.Fatalf("metrics = %#v, want fail-open allow", metrics)
	}
}

func TestRuntimeModeratorFallbackReResolvesAndBlocksSecondCandidate(t *testing.T) {
	const tenantID = "tenant-fallback"
	store := newTestStore(t)
	allowProfile := createRuntimeProfile(t, store, tenantID, "profile-allow", CreateProfileInput{
		Name:            "allow",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"never-match"},
	})
	blockProfile := createRuntimeProfile(t, store, tenantID, "profile-block", CreateProfileInput{
		Name:            "block",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"blocked"},
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-a", allowProfile.ID)
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-b", blockProfile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(nil))
	executor := &runtimeExecutor{calls: make(map[string]int), failAuthID: "auth-a"}
	manager := newRuntimeManager(t, moderator, executor,
		&coreauth.Auth{ID: "auth-a", TenantID: tenantID, Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"provider_key_id": "key-a"}},
		&coreauth.Auth{ID: "auth-b", TenantID: tenantID, Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"provider_key_id": "key-b"}},
	)

	_, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, runtimeOptions(tenantID, "blocked"))
	var authErr *coreauth.Error
	if !errors.As(err, &authErr) || authErr.Code != "content_policy_violation" {
		t.Fatalf("Execute error = %#v, want content policy violation", err)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.calls["auth-a"] != 1 || executor.calls["auth-b"] != 0 {
		t.Fatalf("executor calls = %#v, want first called and blocked second skipped", executor.calls)
	}
}

func TestRuntimeModeratorRequestCacheAvoidsDuplicateModerationCallOnFallback(t *testing.T) {
	const tenantID = "tenant-cache"
	var moderationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		moderationCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"violence":0.01}}]}`))
	}))
	defer server.Close()

	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "profile-shared", CreateProfileInput{
		Name:        "shared",
		Mode:        ModePreBlock,
		KeywordMode: KeywordModeAPIOnly,
		APIKey:      "moderation-secret",
		BaseURL:     server.URL,
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-a", profile.ID)
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-b", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(server.Client()))
	executor := &runtimeExecutor{calls: make(map[string]int), failAuthID: "auth-a"}
	manager := newRuntimeManager(t, moderator, executor,
		&coreauth.Auth{ID: "auth-a", TenantID: tenantID, Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"provider_key_id": "key-a"}},
		&coreauth.Auth{ID: "auth-b", TenantID: tenantID, Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"provider_key_id": "key-b"}},
	)

	if _, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "model"}, runtimeOptions(tenantID, "safe")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if moderationCalls.Load() != 1 {
		t.Fatalf("moderation calls = %d, want 1", moderationCalls.Load())
	}
	if moderator.Metrics().CacheHits != 1 {
		t.Fatalf("metrics = %#v, want one cache hit", moderator.Metrics())
	}
}

func TestRuntimeModeratorUsesLastGoodResolutionOnStoreError(t *testing.T) {
	profile, err := NewProfile("tenant-last-good", "profile-last-good", CreateProfileInput{
		Name:            "last good",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"blocked"},
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	var call atomic.Int32
	resolver := &runtimeResolverStub{fn: func() (Profile, string, error) {
		if call.Add(1) == 1 {
			return profile, ChannelTypeProviderKey, nil
		}
		return Profile{}, "", errors.New("database unavailable")
	}}
	moderator := NewRequestModerator(resolver, NewEvaluator(nil))
	auth := &coreauth.Auth{ID: "auth", TenantID: profile.TenantID, Attributes: map[string]string{"provider_key_id": "key"}}
	for i := 0; i < 2; i++ {
		if result := moderator.Moderate(context.Background(), auth, runtimeOptions(profile.TenantID, "blocked")); !result.Blocked {
			t.Fatalf("call %d was not blocked", i+1)
		}
	}
	if moderator.Metrics().Errors != 1 {
		t.Fatalf("metrics = %#v, want last-good store error", moderator.Metrics())
	}
}

// End to end through the runtime: a keyword in a tool result used to pass
// unmoderated, zero-width space or not. It is now blocked, and the request log
// records which class of text it was found in.
func TestRuntimeModeratorBlocksKeywordHiddenInToolResult(t *testing.T) {
	const tenantID = "tenant-tool-result"
	store := newTestStore(t)
	profile := createRuntimeProfile(t, store, tenantID, "profile-tool-result", CreateProfileInput{
		Name:            "tool result",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordOnly,
		BlockedKeywords: []string{"forbidden"},
	})
	bindRuntimeChannel(t, store, tenantID, ChannelTypeProviderKey, "key-a", profile.ID)
	moderator := NewRequestModerator(store, NewEvaluator(nil))
	auth := &coreauth.Auth{ID: "auth-a", TenantID: tenantID, Attributes: map[string]string{"provider_key_id": "key-a"}}
	ctx, ginCtx := snapshotContext(t)
	result := moderator.Moderate(ctx, auth, cliproxyexecutor.Options{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"for\u200bbidden"},{"type":"text","text":"summarize"}]}]}`),
		SourceFormat:    sdktranslator.FormatClaude,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	})
	if !result.Blocked {
		t.Fatal("keyword in a tool result was not blocked")
	}
	snapshot, ok := RuntimeSnapshotFromGin(ginCtx)
	if !ok || snapshot.MatchedKeyword != "forbidden" || snapshot.MatchedSource != ClassToolOutput.String() {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

// keyword_and_api: a keyword hit never reaches the backend, so it is neither
// in flight nor a latency sample, and the runtime's pre-check reuses the
// evaluator's scan instead of normalizing the request twice.
func TestRuntimeModeratorKeywordHitSkipsBackendMetrics(t *testing.T) {
	const tenantID = "tenant-keyword-and-api"
	profile, err := NewProfile(tenantID, "profile-keyword-and-api", CreateProfileInput{
		Name:            "keyword and api",
		Mode:            ModePreBlock,
		KeywordMode:     KeywordModeKeywordAndAPI,
		BlockedKeywords: []string{"forbidden"},
		APIKey:          "moderation-secret",
	}, time.Now())
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	moderator := NewRequestModerator(&runtimeResolverStub{fn: func() (Profile, string, error) {
		return profile, ChannelTypeProviderKey, nil
	}}, NewEvaluator(nil))
	auth := &coreauth.Auth{ID: "auth", TenantID: tenantID, Attributes: map[string]string{"provider_key_id": "key"}}
	body := `{"messages":[{"role":"assistant","content":"a forbidden reply"},{"role":"user","content":"continue"}]}`
	result := moderator.Moderate(context.Background(), auth, cliproxyexecutor.Options{
		OriginalRequest: []byte(body),
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	})
	if !result.Blocked {
		t.Fatal("keyword in assistant history was not blocked")
	}
	if metrics := moderator.Metrics(); metrics.Blocks != 1 || metrics.LatencySamples != 0 || metrics.InFlight != 0 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestDecisionCacheKeyCoversClassesAndTurns(t *testing.T) {
	profile := Profile{ID: "profile", Version: 3}
	base := decisionCacheKey(profile, NewInput([]Segment{{Class: ClassPrimary, User: true, Turn: 1, Text: "same"}}))
	if same := decisionCacheKey(profile, NewInput([]Segment{{Class: ClassPrimary, User: true, Turn: 1, Text: "same"}})); same != base {
		t.Fatal("identical inputs got different cache keys")
	}
	for _, other := range []Segment{
		{Class: ClassToolOutput, Turn: 1, Text: "same"},
		{Class: ClassPrimary, User: false, Turn: 1, Text: "same"},
		{Class: ClassPrimary, User: true, Turn: 2, Text: "same"},
	} {
		if decisionCacheKey(profile, NewInput([]Segment{other})) == base {
			t.Fatalf("%+v shares the cache key of a different input", other)
		}
	}
}

package contentmoderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// wordResolverStub serves fixed policies and remembers who asked.
type wordResolverStub struct {
	mu       sync.Mutex
	policies WordPolicies
	err      error
	tenants  []string
	keys     []string
}

func (r *wordResolverStub) LoadWordPolicies(_ context.Context, tenantID, keyID string) (WordPolicies, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants, r.keys = append(r.tenants, tenantID), append(r.keys, keyID)
	return r.policies, r.err
}

func guardModerator(resolver WordPolicyResolver) *RuntimeModerator {
	moderator := NewRequestModerator(nil, NewEvaluator(nil))
	moderator.words = resolver
	return moderator
}

func chatRequest(text string) []byte {
	return []byte(`{"model":"m","messages":[{"role":"user","content":` + quoteJSON(text) + `}]}`)
}

func guardOptions(tenantID string, body []byte) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	return cliproxyexecutor.Request{Model: "m", Payload: body}, cliproxyexecutor.Options{
		OriginalRequest: body,
		SourceFormat:    sdktranslator.FormatOpenAI,
		Metadata:        map[string]any{cliproxyexecutor.TenantMetadataKey: tenantID},
	}
}

func TestGuardRequestIsInertWithoutPolicies(t *testing.T) {
	body := chatRequest("hello bad world")
	req, opts := guardOptions("tenant-a", body)
	if result := guardModerator(nil).GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload != nil || result.Reply != nil {
		t.Fatalf("result = %+v", result)
	}
	resolver := &wordResolverStub{}
	if result := guardModerator(resolver).GuardRequest(context.Background(), req, opts); result.Blocked || result.Payload != nil || result.Reply != nil {
		t.Fatalf("result = %+v", result)
	}
	if len(resolver.tenants) != 1 || resolver.tenants[0] != "tenant-a" {
		t.Fatalf("policies were loaded for %v", resolver.tenants)
	}
}

func TestGuardRequestBlocksWithTheConfiguredMessage(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.BlockMessage = "not allowed" }, kw("bad"))
	ctx, ginCtx := snapshotContext(t)
	req, opts := guardOptions("tenant-a", chatRequest("hello bad world"))
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenant}}).GuardRequest(ctx, req, opts)
	if !result.Blocked || result.Message != "not allowed" || result.HTTPStatus != http.StatusForbidden {
		t.Fatalf("result = %+v", result)
	}
	snapshot, ok := WordGuardSnapshotFromGin(ginCtx)
	if !ok {
		t.Fatal("a blocked request left nothing for the request log")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(encoded)
	if gjson.Get(doc, "action").String() != "block" || !gjson.Get(doc, "blocked").Bool() || gjson.Get(doc, "matches.0.layer").String() != "tenant" || gjson.Get(doc, "matches.0.rule_id").String() == "" {
		t.Fatalf("snapshot = %s", doc)
	}
}

func TestGuardRequestReplaceHandsBackOneRewrittenBody(t *testing.T) {
	tenant := tenantOf(t, WordActionReplace, kw("bad"))
	body := chatRequest("hello bad world")
	req, opts := guardOptions("tenant-a", body)
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenant}}).GuardRequest(context.Background(), req, opts)
	want := string(chatRequest("hello *** world"))
	if result.Blocked || string(result.Payload) != want || string(result.OriginalRequest) != want {
		t.Fatalf("result = %+v", result)
	}
	// The executors use one translation for both when they are one slice.
	if !sameStorage(result.Payload, result.OriginalRequest) {
		t.Fatal("payload and original request no longer share storage")
	}
	if string(body) != string(chatRequest("hello bad world")) {
		t.Fatal("the caller's body was modified")
	}
	if result.Reply != nil {
		t.Fatal("reply filtering is off by default")
	}
}

func TestGuardRequestLogOnlyLeavesTheBody(t *testing.T) {
	tenant := tenantOf(t, WordActionLog, kw("bad"))
	ctx, ginCtx := snapshotContext(t)
	req, opts := guardOptions("tenant-a", chatRequest("hello bad world"))
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenant}}).GuardRequest(ctx, req, opts)
	if result.Blocked || result.Payload != nil || result.OriginalRequest != nil {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := WordGuardSnapshotFromGin(ginCtx); !ok {
		t.Fatal("a hit under the log action must reach the request log")
	}
}

func TestGuardRequestRecordsNothingForACleanRequest(t *testing.T) {
	ctx, ginCtx := snapshotContext(t)
	req, opts := guardOptions("tenant-a", chatRequest("all good here"))
	guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenantOf(t, WordActionReplace, kw("bad"))}}).GuardRequest(ctx, req, opts)
	if _, ok := WordGuardSnapshotFromGin(ginCtx); ok {
		t.Fatal("a request without hits must not add to the request log")
	}
}

func TestGuardRequestLoadsTheKeyPolicyOfTheAuthenticatedKey(t *testing.T) {
	ctx, ginCtx := snapshotContext(t)
	ginCtx.Set("accessMetadata", map[string]string{"api-key-id": " key-7 ", "tenant-id": "tenant-a"})
	resolver := &wordResolverStub{}
	req, opts := guardOptions("tenant-a", chatRequest("hi"))
	guardModerator(resolver).GuardRequest(ctx, req, opts)
	if len(resolver.keys) != 1 || resolver.keys[0] != "key-7" {
		t.Fatalf("key ids = %q", resolver.keys)
	}
}

func TestGuardRequestUsesTheSystemTenantWhenNoneIsSet(t *testing.T) {
	resolver := &wordResolverStub{}
	req := cliproxyexecutor.Request{Model: "m", Payload: chatRequest("hi")}
	guardModerator(resolver).GuardRequest(context.Background(), req, cliproxyexecutor.Options{OriginalRequest: req.Payload, SourceFormat: sdktranslator.FormatOpenAI})
	if len(resolver.tenants) != 1 || resolver.tenants[0] != coreauth.NormalizedTenantID("") {
		t.Fatalf("tenants = %q", resolver.tenants)
	}
}

func TestGuardRequestPlatformRulesApplyWhateverTheTenantSays(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) { p.Action = WordActionBlock }, kw("platformword"))
	// A tenant that wants only to log, with a key that tries to turn itself off.
	tenant := tenantOf(t, WordActionLog, kw("other"))
	key := policyOf(t, WordScopeKey, "key-1", func(p *WordPolicy) { p.Action = WordActionLog; p.Enabled = false }, kw("more"))
	req, opts := guardOptions("tenant-a", chatRequest("this has platformword in it"))
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Platform: platform, Tenant: tenant, Key: key}}).GuardRequest(context.Background(), req, opts)
	if !result.Blocked {
		t.Fatalf("a tenant or key switched the platform policy off: %+v", result)
	}
}

func TestGuardRequestFailsOpenButKeepsWhatItLastSaw(t *testing.T) {
	outage := errors.New("database down")
	req, opts := guardOptions("tenant-a", chatRequest("hello bad world"))

	// Nothing known: the request passes.
	result := guardModerator(&wordResolverStub{err: outage}).GuardRequest(context.Background(), req, opts)
	if result.Blocked || result.Payload != nil {
		t.Fatalf("result = %+v", result)
	}

	// The store hands back the policies it remembers together with the error.
	ctx, ginCtx := snapshotContext(t)
	remembered := WordPolicies{Tenant: tenantOf(t, WordActionBlock, kw("bad"))}
	result = guardModerator(&wordResolverStub{policies: remembered, err: outage}).GuardRequest(ctx, req, opts)
	if !result.Blocked {
		t.Fatalf("remembered policies were not enforced during an outage: %+v", result)
	}
	snapshot, _ := WordGuardSnapshotFromGin(ginCtx)
	encoded, _ := json.Marshal(snapshot)
	if gjson.GetBytes(encoded, "policy_error").String() != "policy_store_error" {
		t.Fatalf("snapshot = %s", encoded)
	}
}

func TestGuardRequestOffersAReplyFilterOnlyWhenAskedFor(t *testing.T) {
	asking := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.ReplyFilter = WordActionReplace, boolPtr(true)
	}, kw("bad"))
	req, opts := guardOptions("tenant-a", chatRequest("a clean request"))
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: asking}}).GuardRequest(context.Background(), req, opts)
	if result.Reply == nil {
		t.Fatal("no reply filter although the policy asked for one")
	}
	notAsking := tenantOf(t, WordActionReplace, kw("bad"))
	result = guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: notAsking}}).GuardRequest(context.Background(), req, opts)
	if result.Reply != nil {
		t.Fatal("a reply filter although the policy did not ask for one")
	}
}

func TestGuardRequestChecksAPayloadThatIsNotTheOriginalRequest(t *testing.T) {
	tenant := tenantOf(t, WordActionReplace, kw("bad"))
	original := chatRequest("first bad")
	payload := chatRequest("second bad")
	req := cliproxyexecutor.Request{Model: "m", Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: original, SourceFormat: sdktranslator.FormatOpenAI}
	result := guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenant}}).GuardRequest(context.Background(), req, opts)
	if string(result.OriginalRequest) != string(chatRequest("first ***")) || string(result.Payload) != string(chatRequest("second ***")) {
		t.Fatalf("result = %s / %s", result.OriginalRequest, result.Payload)
	}

	// The same text in two slices is rewritten once and handed back as one.
	twin := append([]byte(nil), original...)
	result = guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: tenant}}).GuardRequest(context.Background(),
		cliproxyexecutor.Request{Model: "m", Payload: twin}, opts)
	if string(result.OriginalRequest) != string(chatRequest("first ***")) || string(result.Payload) != string(chatRequest("first ***")) {
		t.Fatalf("result = %s / %s", result.OriginalRequest, result.Payload)
	}

	// A payload that blocks blocks the request.
	blocker := tenantOf(t, WordActionBlock, kw("second"))
	result = guardModerator(&wordResolverStub{policies: WordPolicies{Tenant: blocker}}).GuardRequest(context.Background(), req, opts)
	if !result.Blocked {
		t.Fatalf("a body the executors would translate was never checked: %+v", result)
	}
}

// guardExecutor records what the conductor hands it and answers with a canned
// reply, so the whole path from the stored policy to the client's bytes runs.
type guardExecutor struct {
	mu     sync.Mutex
	bodies []string
	reply  string
	events []string
}

func (e *guardExecutor) Identifier() string { return "word-guard-e2e" }

func (e *guardExecutor) Execute(_ context.Context, _ *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.bodies = append(e.bodies, string(req.Payload))
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(e.reply), Headers: http.Header{"Content-Length": {"1"}}}, nil
}

func (e *guardExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.bodies = append(e.bodies, string(req.Payload))
	e.mu.Unlock()
	out := make(chan cliproxyexecutor.StreamChunk, len(e.events))
	for _, event := range e.events {
		out <- cliproxyexecutor.StreamChunk{Payload: []byte(event)}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func (e *guardExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *guardExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *guardExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newGuardManager(t *testing.T, store *Store, executor *guardExecutor, tenants ...string) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, runtimeOrderedSelector{}, nil)
	manager.SetRequestModerator(NewRequestModerator(store, NewEvaluator(nil)))
	for _, tenant := range tenants {
		manager.RegisterExecutorForTenant(tenant, executor)
		auth := &coreauth.Auth{ID: "acct-" + tenant, TenantID: tenant, Provider: executor.Identifier(), Status: coreauth.StatusActive}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return manager
}

// The policy a tenant saved decides what its upstream receives and what its
// client reads back, through the real store and the real conductor.
func TestWordGuardEndToEndThroughStoreAndConductor(t *testing.T) {
	store := newTestStore(t)
	const tenant = "tenant-e2e"
	savedWordPolicy(t, store, tenant, WordPolicy{
		ScopeType: WordScopeTenant, Enabled: true, Action: WordActionReplace, ReplyFilter: boolPtr(true), InheritTenant: true,
		Rules: []WordRule{kw("badword")},
	}, nil)
	executor := &guardExecutor{
		reply: `{"type":"message","content":[{"type":"text","text":"you said badword"}]}`,
		events: []string{
			claudeText(0, "echo bad"), claudeText(0, "word!"), claudeStop,
		},
	}
	manager := newGuardManager(t, store, executor, tenant, "tenant-other")
	body := chatRequest("please say badword")
	opts := cliproxyexecutor.Options{OriginalRequest: body, SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenant}}

	resp, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "m", Payload: body}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(executor.bodies[0]); got != string(chatRequest("please say ***")) {
		t.Fatalf("the upstream received %s", got)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "you said ***" {
		t.Fatalf("the client reads %q", got)
	}
	if resp.Headers.Get("Content-Length") != "" {
		t.Fatalf("headers = %v", resp.Headers)
	}

	stream, err := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "m", Payload: body},
		cliproxyexecutor.Options{Stream: true, OriginalRequest: body, SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: tenant}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		chunks = append(chunks, chunk.Payload)
	}
	if got := readerOf(sdktranslator.FormatClaude, chunks); got != "echo ***!" {
		t.Fatalf("the client reads %q", got)
	}
	if strings.Count(joined(chunks), "event: ") != 3 {
		t.Fatalf("events changed: %q", joined(chunks))
	}

	// Another tenant has no policy and sees none of this.
	other := cliproxyexecutor.Options{OriginalRequest: body, SourceFormat: sdktranslator.FormatClaude, Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: "tenant-other"}}
	if _, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "m", Payload: body}, other); err != nil {
		t.Fatal(err)
	}
	if got := executor.bodies[len(executor.bodies)-1]; got != string(body) {
		t.Fatalf("another tenant's request was rewritten: %s", got)
	}
}

func TestWordGuardEndToEndBlocksBeforeTheUpstream(t *testing.T) {
	store := newTestStore(t)
	savedWordPolicy(t, store, coreauth.NormalizedTenantID(""), WordPolicy{
		ScopeType: WordScopePlatform, Enabled: true, Action: WordActionBlock, InheritTenant: true, Rules: []WordRule{kw("forbidden")},
	}, nil)
	executor := &guardExecutor{}
	manager := newGuardManager(t, store, executor, "any-tenant")
	body := chatRequest("a forbidden request")
	_, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: "m", Payload: body},
		cliproxyexecutor.Options{OriginalRequest: body, SourceFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{cliproxyexecutor.TenantMetadataKey: "any-tenant"}})
	requireContentPolicyViolation(t, err, http.StatusForbidden, DefaultBlockMessage)
	if len(executor.bodies) != 0 {
		t.Fatalf("the upstream saw %d requests", len(executor.bodies))
	}
}

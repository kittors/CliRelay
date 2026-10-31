package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/routetrace"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

// vmOutcome is how the upstream answers one call for a model.
type vmOutcome struct {
	// err refuses the call outright (an HTTP error before any stream opens).
	err error
	// payload is the answer; streamErr, for a stream, follows it, or comes
	// alone when there is no payload (a failure before the first byte).
	payload   string
	streamErr error
}

func vmFail(status int) vmOutcome {
	return vmOutcome{err: &coreauth.Error{Code: "upstream_error", Message: "upstream refused", HTTPStatus: status}}
}

// vmExecutor answers by model, whichever account runs it: the n-th call for a
// model takes the n-th outcome scripted for it, and the last one repeats.
type vmExecutor struct {
	outcomes map[string][]vmOutcome
	// onCall runs before a call is answered.
	onCall func(model string)

	mu    sync.Mutex
	calls []string
	seen  map[string]int
}

func (e *vmExecutor) Identifier() string { return "vmtest" }

func (e *vmExecutor) next(req coreexecutor.Request) vmOutcome {
	model := req.Model
	if e.onCall != nil {
		e.onCall(model)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, model)
	if e.seen == nil {
		e.seen = make(map[string]int)
	}
	script := e.outcomes[model]
	if len(script) == 0 {
		return vmOutcome{payload: "from-" + model}
	}
	n := min(e.seen[model], len(script)-1)
	e.seen[model]++
	return script[n]
}

func (e *vmExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	out := e.next(req)
	if out.err != nil {
		return coreexecutor.Response{}, out.err
	}
	return coreexecutor.Response{Payload: []byte(out.payload)}, nil
}

func (e *vmExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	out := e.next(req)
	if out.err != nil {
		return nil, out.err
	}
	ch := make(chan coreexecutor.StreamChunk, 2)
	if out.payload != "" {
		ch <- coreexecutor.StreamChunk{Payload: []byte(out.payload)}
	}
	if out.streamErr != nil {
		ch <- coreexecutor.StreamChunk{Err: out.streamErr}
	}
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *vmExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *vmExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *vmExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *vmExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// newVirtualModelManager gives each model its own account, so that one model
// failing leaves the others' accounts untouched.
func newVirtualModelManager(t *testing.T, exec coreauth.ProviderExecutor, models ...string) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	for _, model := range models {
		id := "vm-" + t.Name() + "-" + model
		if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "vmtest", Status: coreauth.StatusActive}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "vmtest", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return manager
}

// virtualModelContext is a request context whose gin context carries a
// planner for plan, and a route trace.
func virtualModelContext(plan *VirtualModelPlan) (context.Context, *gin.Context, *httptest.ResponseRecorder, *routetrace.Recorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(VirtualModelPlannerContextKey, VirtualModelPlanner(func(model string) *VirtualModelPlan {
		if plan == nil || model != plan.Name {
			return nil
		}
		copied := *plan
		copied.Members = append([]VirtualModelMember(nil), plan.Members...)
		return &copied
	}))
	trace := routetrace.New()
	ctx := routetrace.WithRecorder(context.WithValue(context.Background(), util.ContextKeyGin, c), trace)
	return ctx, c, recorder, trace
}

func plan(name string, models ...string) *VirtualModelPlan {
	out := &VirtualModelPlan{Name: name}
	for _, model := range models {
		out.Members = append(out.Members, VirtualModelMember{Model: model})
	}
	return out
}

func memberSteps(trace *routetrace.Recorder) []routetrace.Step {
	var out []routetrace.Step
	for _, step := range trace.Snapshot().Steps {
		if step.Kind == routetrace.KindMember {
			out = append(out, step)
		}
	}
	return out
}

func TestVirtualModelFallsBackToTheNextMember(t *testing.T) {
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {vmFail(http.StatusServiceUnavailable)}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta", "gamma"))
	ctx, _, _, trace := virtualModelContext(plan("smart", "alpha", "beta", "gamma"))

	resp, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "smart", []byte(`{"model":"smart"}`), "")
	if errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager: %v", errMsg.Error)
	}
	if string(resp) != "from-beta" {
		t.Fatalf("resp = %q, want beta's answer", resp)
	}
	if calls := exec.Calls(); strings.Join(calls, ",") != "alpha,beta" {
		t.Fatalf("calls = %v, want alpha then beta and nothing after the member that served", calls)
	}
	for header, want := range map[string]string{
		virtualModelHeader:   "smart",
		modelRequestedHeader: "smart",
		modelRoutedHeader:    "beta",
		modelFallbackHeader:  "alpha",
	} {
		if got := headers.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}

	steps := memberSteps(trace)
	if len(steps) != 2 || steps[0].Reason != routetrace.MemberFirst || steps[0].Model != "alpha" || steps[0].Member != 1 || steps[0].Members != 3 {
		t.Fatalf("member steps = %+v, want alpha first", steps)
	}
	if got := steps[1]; got.Reason != routetrace.MemberFallback || got.Model != "beta" || got.Status != http.StatusServiceUnavailable || got.Error == "" {
		t.Fatalf("fallback step = %+v, want beta with alpha's failure", got)
	}
	if model := trace.Live().Model; model != "smart" {
		t.Fatalf("trace model = %q, want the virtual model the client asked for", model)
	}
}

// A request the upstream refused for what it asks would be refused by the
// next member too; and a client that went away needs no other member.
func TestVirtualModelStopsWhenMovingOnCannotHelp(t *testing.T) {
	invalid := &coreauth.Error{HTTPStatus: http.StatusBadRequest, Code: "invalid_request_error", Message: "invalid_request_error: messages: field required"}
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {{err: invalid}}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))

	_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "smart", []byte(`{"model":"smart"}`), "")
	if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("errMsg = %+v, want the 400 of the malformed request", errMsg)
	}
	if got := errMsg.Addon.Get(virtualModelHeader); got != "smart" {
		t.Fatalf("error header %s = %q, want the virtual model", virtualModelHeader, got)
	}
	if calls := exec.Calls(); strings.Join(calls, ",") != "alpha" {
		t.Fatalf("calls = %v, want no member after a request fault", calls)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	gone := &vmExecutor{
		outcomes: map[string][]vmOutcome{"alpha": {{err: context.Canceled}}},
		onCall:   func(string) { cancel() },
	}
	handler = NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, gone, "alpha", "beta"))
	ctx, c, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))
	ctx = context.WithValue(cancelCtx, util.ContextKeyGin, c)
	if _, _, errMsg = handler.ExecuteWithAuthManager(ctx, "openai", "smart", []byte(`{}`), ""); errMsg == nil {
		t.Fatal("a request whose client went away must fail")
	}
	if calls := gone.Calls(); strings.Join(calls, ",") != "alpha" {
		t.Fatalf("calls = %v, want no member once the client is gone", calls)
	}
}

func TestVirtualModelPicksTheFirstMemberByWeight(t *testing.T) {
	previous := virtualModelPick
	t.Cleanup(func() { virtualModelPick = previous })
	var asked int
	virtualModelPick = func(n int) int { asked = n; return 1 }

	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"gamma": {vmFail(http.StatusBadGateway)}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta", "gamma"))
	weighted := &VirtualModelPlan{Name: "spread", Weighted: true, Members: []VirtualModelMember{
		{Model: "alpha", Weight: 1}, {Model: "beta"}, {Model: "gamma", Weight: 3},
	}}
	ctx, _, _, trace := virtualModelContext(weighted)

	resp, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "spread", []byte(`{}`), "")
	if errMsg != nil || string(resp) != "from-alpha" {
		t.Fatalf("resp = %q, %+v, want alpha after gamma failed", resp, errMsg)
	}
	if asked != 4 {
		t.Fatalf("pick asked for %d, want the total weight 4", asked)
	}
	// 1 falls past alpha's share into gamma's; the others follow in order.
	if calls := exec.Calls(); strings.Join(calls, ",") != "gamma,alpha" {
		t.Fatalf("calls = %v, want gamma picked by weight, then alpha", calls)
	}
	if steps := memberSteps(trace); len(steps) != 2 || steps[0].Reason != routetrace.MemberWeighted || steps[0].Member != 3 {
		t.Fatalf("member steps = %+v, want gamma picked by weight", steps)
	}
}

func TestVirtualModelPlanOrder(t *testing.T) {
	never := func(int) int { t.Fatal("pick must not be asked"); return 0 }
	listed := &VirtualModelPlan{Members: []VirtualModelMember{{Model: "a"}, {Model: "b", Skip: routetrace.MemberDisabled}, {Model: "c"}}}
	if order, reason := listed.order(never); len(order) != 2 || order[0] != 0 || order[1] != 2 || reason != routetrace.MemberFirst {
		t.Fatalf("order = %v %q, want the usable members in the listed order", order, reason)
	}
	unweighted := &VirtualModelPlan{Weighted: true, Members: []VirtualModelMember{{Model: "a"}, {Model: "b"}}}
	if order, reason := unweighted.order(never); len(order) != 2 || reason != routetrace.MemberFirst {
		t.Fatalf("order = %v %q, want the listed order when no usable member has a weight", order, reason)
	}
	// The weighted member the request may not use cannot be picked.
	skipped := &VirtualModelPlan{Weighted: true, Members: []VirtualModelMember{{Model: "a", Weight: 5, Skip: routetrace.MemberNotAllowed}, {Model: "b", Weight: 1}, {Model: "c"}}}
	if order, reason := skipped.order(func(n int) int { return n - 1 }); len(order) != 2 || order[0] != 1 || reason != routetrace.MemberWeighted {
		t.Fatalf("order = %v %q, want b picked among the usable members", order, reason)
	}
}

// Members the request may not use, and ones no account serves, are passed
// over before any is tried, and the route trace says why.
func TestVirtualModelPassesOverMembersItCannotUse(t *testing.T) {
	exec := &vmExecutor{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	members := &VirtualModelPlan{Name: "smart", Members: []VirtualModelMember{
		{Model: "alpha", Skip: routetrace.MemberNotAllowed, Detail: "the API key may not use channel group \"team\""},
		{Model: "nosuch"},
		{Model: "beta"},
	}}
	ctx, _, _, trace := virtualModelContext(members)

	resp, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
	if errMsg != nil || string(resp) != "from-beta" {
		t.Fatalf("resp = %q, %+v, want beta", resp, errMsg)
	}
	if got := headers.Get(modelFallbackHeader); got != "" {
		t.Fatalf("%s = %q, want none: no member failed", modelFallbackHeader, got)
	}
	steps := memberSteps(trace)
	if len(steps) != 3 || steps[0].Reason != routetrace.MemberNotAllowed || steps[1].Reason != routetrace.MemberNoProvider || steps[2].Reason != routetrace.MemberFirst || steps[2].Model != "beta" {
		t.Fatalf("member steps = %+v, want alpha refused, nosuch unserved, beta first", steps)
	}
	if calls := exec.Calls(); strings.Join(calls, ",") != "beta" {
		t.Fatalf("calls = %v, want only beta", calls)
	}

	for name, tc := range map[string]struct {
		plan   *VirtualModelPlan
		status int
	}{
		"nothing serves": {plan("dead", "nosuch", "nothing"), http.StatusBadGateway},
		"nothing allowed": {&VirtualModelPlan{Name: "dead", Members: []VirtualModelMember{
			{Model: "alpha", Skip: routetrace.MemberDisabled, Detail: "model \"alpha\" is disabled"},
		}}, http.StatusForbidden},
	} {
		ctx, _, _, _ := virtualModelContext(tc.plan)
		_, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "dead", []byte(`{}`), "")
		if errMsg == nil || errMsg.StatusCode != tc.status || !strings.Contains(errMsg.Error.Error(), `virtual model "dead"`) {
			t.Fatalf("%s: errMsg = %+v, want %d naming the virtual model", name, errMsg, tc.status)
		}
	}
}

func TestVirtualModelCountsOnTheFirstMemberThatCan(t *testing.T) {
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {vmFail(http.StatusTooManyRequests)}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))

	resp, _, errMsg := handler.ExecuteCountWithAuthManager(ctx, "claude", "smart", []byte(`{}`), "")
	if errMsg != nil || string(resp) != "from-beta" {
		t.Fatalf("resp = %q, %+v, want beta's count", resp, errMsg)
	}
}

// Without a planner, or for a name that is no virtual model, nothing changes.
func TestRealModelsRunAsBefore(t *testing.T) {
	exec := &vmExecutor{}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha"))
	ctx, _, _, trace := virtualModelContext(plan("smart", "alpha"))

	resp, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "alpha", []byte(`{}`), "")
	if errMsg != nil || string(resp) != "from-alpha" || headers.Get(virtualModelHeader) != "" {
		t.Fatalf("resp = %q, headers = %v, %+v, want alpha untouched", resp, headers, errMsg)
	}
	if steps := memberSteps(trace); len(steps) != 0 {
		t.Fatalf("member steps = %+v, want none for a real model", steps)
	}
	resp, _, errMsg = handler.ExecuteWithAuthManager(context.Background(), "openai", "alpha", []byte(`{}`), "")
	if errMsg != nil || string(resp) != "from-alpha" {
		t.Fatalf("without a gin context: resp = %q, %+v", resp, errMsg)
	}
}

func TestVirtualModelStreamMovesOnBeforeHandingOver(t *testing.T) {
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {vmFail(http.StatusServiceUnavailable)}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, c, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))

	dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
	if startErr != nil {
		t.Fatalf("StartStreamWithAuthManager: %v", startErr.Error)
	}
	if got := c.Writer.Header().Get(modelRoutedHeader); got != "beta" {
		t.Fatalf("%s = %q, want beta, the member the stream started on", modelRoutedHeader, got)
	}
	if got := c.Writer.Header().Get(modelFallbackHeader); got != "alpha" {
		t.Fatalf("%s = %q, want alpha", modelFallbackHeader, got)
	}
	if out := drainStream(t, dataChan, errChan); out.err != nil || out.payload != "from-beta" {
		t.Fatalf("outcome = %+v, want beta's stream", out)
	}
}

// A stream that fails before its first byte, once its own account retries
// are used up, moves to the next member; the client sees one clean stream.
func TestVirtualModelStreamMovesOnBeforeTheFirstByte(t *testing.T) {
	broke := &coreauth.Error{Code: "upstream_error", Message: "upstream broke", HTTPStatus: http.StatusBadGateway}
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {{streamErr: broke}}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, _, _, trace := virtualModelContext(plan("smart", "alpha", "beta"))

	dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
	if startErr != nil {
		t.Fatalf("StartStreamWithAuthManager: %v", startErr.Error)
	}
	if out := drainStream(t, dataChan, errChan); out.err != nil || out.payload != "from-beta" {
		t.Fatalf("outcome = %+v, want beta's stream after alpha broke", out)
	}
	if calls := exec.Calls(); len(calls) != 2 || calls[0] != "alpha" || calls[1] != "beta" {
		t.Fatalf("calls = %v, want alpha then beta", calls)
	}
	steps := memberSteps(trace)
	if len(steps) != 2 || steps[1].Reason != routetrace.MemberFallback || steps[1].Status != http.StatusBadGateway {
		t.Fatalf("member steps = %+v, want the fallback to beta after alpha's 502", steps)
	}
}

func TestVirtualModelStreamNeverMovesOnAfterTheFirstByte(t *testing.T) {
	broke := &coreauth.Error{Code: "upstream_error", Message: "upstream broke", HTTPStatus: http.StatusBadGateway}
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {{payload: "partial", streamErr: broke}}}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))

	dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
	if startErr != nil {
		t.Fatalf("StartStreamWithAuthManager: %v", startErr.Error)
	}
	out := drainStream(t, dataChan, errChan)
	if out.payload != "partial" || out.status != http.StatusBadGateway {
		t.Fatalf("outcome = %+v, want the partial answer and alpha's failure", out)
	}
	if calls := exec.Calls(); strings.Join(calls, ",") != "alpha" {
		t.Fatalf("calls = %v, want no member after the first byte", calls)
	}
}

func TestVirtualModelStreamReportsTheLastFailure(t *testing.T) {
	exec := &vmExecutor{outcomes: map[string][]vmOutcome{
		"alpha": {vmFail(http.StatusServiceUnavailable)},
		"beta":  {vmFail(http.StatusTooManyRequests)},
	}}
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, newVirtualModelManager(t, exec, "alpha", "beta"))
	ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))

	_, _, _, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
	if startErr == nil || startErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("startErr = %+v, want beta's 429, the last member's failure", startErr)
	}
}

// Whose accounts all cool, a member would make the scheduler wait for the one
// back soonest. With another member after it the request moves on at once; the
// last member, with nowhere else to go, waits as a real model does.
func TestVirtualModelDoesNotWaitForACoolingMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		exec := &vmExecutor{}
		manager := newVirtualModelManager(t, exec, "alpha", "beta")
		manager.SetConfig(&sdkconfig.Config{Routing: sdkconfig.RoutingConfig{
			ChannelGroups: []sdkconfig.RoutingChannelGroup{{Name: "default", BusyWaitMaxSeconds: new(60)}},
		}})
		cooling := 15 * time.Second
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID: "vm-" + t.Name() + "-alpha", Provider: "vmtest", Model: "alpha",
			Error:      &coreauth.Error{HTTPStatus: http.StatusServiceUnavailable, Message: "upstream unavailable"},
			RetryAfter: &cooling,
		})
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)

		ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))
		start := time.Now()
		resp, headers, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "smart", []byte(`{}`), "")
		if errMsg != nil || string(resp) != "from-beta" {
			t.Fatalf("resp = %q, %+v, want beta while alpha cools", resp, errMsg)
		}
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("waited %s for alpha's accounts, want none with beta after it", waited)
		}
		if got := headers.Get(modelFallbackHeader); got != "alpha" {
			t.Fatalf("%s = %q, want alpha", modelFallbackHeader, got)
		}

		ctx, _, _, _ = virtualModelContext(plan("solo", "alpha"))
		start = time.Now()
		resp, _, errMsg = handler.ExecuteWithAuthManager(ctx, "openai", "solo", []byte(`{}`), "")
		if errMsg != nil || string(resp) != "from-alpha" {
			t.Fatalf("resp = %q, %+v, want alpha once it recovers", resp, errMsg)
		}
		if waited := time.Since(start); waited < cooling {
			t.Fatalf("waited %s, want the %s until alpha recovers", waited, cooling)
		}
		if calls := exec.Calls(); strings.Join(calls, ",") != "beta,alpha" {
			t.Fatalf("calls = %v, want beta, then alpha after its cooldown", calls)
		}
	})
}

// guardCounter is a request guard that counts how often it looks at a body.
type guardCounter struct {
	mu    sync.Mutex
	calls int
}

func (g *guardCounter) Moderate(context.Context, *coreauth.Auth, coreexecutor.Options) coreauth.RequestModerationResult {
	return coreauth.RequestModerationResult{}
}

func (g *guardCounter) GuardRequest(context.Context, coreexecutor.Request, coreexecutor.Options) coreauth.RequestGuardResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return coreauth.RequestGuardResult{}
}

// The members are executions of one client request: the request guard reads
// its body once, whichever member serves it.
func TestVirtualModelGuardsTheRequestOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		exec := &vmExecutor{outcomes: map[string][]vmOutcome{"alpha": {vmFail(http.StatusServiceUnavailable)}}}
		manager := newVirtualModelManager(t, exec, "alpha", "beta")
		guard := &guardCounter{}
		manager.SetRequestModerator(guard)
		handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
		ctx, _, _, _ := virtualModelContext(plan("smart", "alpha", "beta"))
		body := []byte(`{"model":"smart"}`)

		if stream {
			dataChan, _, errChan, startErr := handler.StartStreamWithAuthManager(ctx, "openai", "smart", body, "")
			if startErr != nil {
				t.Fatalf("stream: %v", startErr.Error)
			}
			drainStream(t, dataChan, errChan)
		} else if _, _, errMsg := handler.ExecuteWithAuthManager(ctx, "openai", "smart", body, ""); errMsg != nil {
			t.Fatalf("execute: %v", errMsg.Error)
		}
		if guard.calls != 1 {
			t.Fatalf("stream=%v: guard ran %d times, want once for the two members", stream, guard.calls)
		}
	}
}

// A member of a group runs on the group's path when the request has none,
// and only the members before the last tell the scheduler not to wait.
func TestVirtualModelMemberContext(t *testing.T) {
	ctx, c, _, _ := virtualModelContext(nil)
	run := &virtualModelRun{ctx: ctx, plan: &VirtualModelPlan{}, shared: &memberMetadata{}}

	member := run.memberContext(VirtualModelMember{Group: "team", Model: "alpha"}, true)
	if got := requestExecutionMetadata(member)[coreexecutor.RouteGroupMetadataKey]; got != "team" {
		t.Fatalf("route group = %v, want the member's group", got)
	}
	if !routetrace.CallerDecides(member) {
		t.Fatal("a member with others after it must leave the decision to the caller")
	}
	last := run.memberContext(VirtualModelMember{Model: "alpha"}, false)
	if routetrace.CallerDecides(last) || requestExecutionMetadata(last)[coreexecutor.RouteGroupMetadataKey] != nil {
		t.Fatal("the last member, without a group, must run as the request would")
	}

	// A request on a group's path stays on it.
	c.Set(sdkrouting.GinPathRouteContextKey, &sdkrouting.PathRouteContext{Group: "ops"})
	onPath := run.memberContext(VirtualModelMember{Group: "ops", Model: "alpha"}, false)
	if got := requestExecutionMetadata(onPath)[coreexecutor.RouteGroupMetadataKey]; got != "ops" {
		t.Fatalf("route group = %v, want the request's path group", got)
	}
}

func TestVirtualModelMovesOn(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		msg  *StreamErrorMessage
		want bool
	}{
		{"upstream down", context.Background(), &StreamErrorMessage{StatusCode: 503, Error: errors.New("down")}, true},
		{"rate limited", context.Background(), &StreamErrorMessage{StatusCode: 429, Error: &coreauth.Error{HTTPStatus: 429, Message: "slow down"}}, true},
		{"no error", context.Background(), nil, false},
		{"client closed", context.Background(), &StreamErrorMessage{StatusCode: 499, Error: context.Canceled}, false},
		{"canceled", context.Background(), &StreamErrorMessage{StatusCode: 500, Error: context.Canceled}, false},
		{"context gone", canceled, &StreamErrorMessage{StatusCode: 503, Error: errors.New("down")}, false},
		{"content policy", context.Background(), &StreamErrorMessage{StatusCode: 403, Error: &coreauth.Error{Code: "content_policy_violation", HTTPStatus: 403}}, false},
	}
	for _, tc := range cases {
		if got := virtualModelMovesOn(tc.ctx, tc.msg); got != tc.want {
			t.Errorf("%s: moves on = %v, want %v", tc.name, got, tc.want)
		}
	}
}

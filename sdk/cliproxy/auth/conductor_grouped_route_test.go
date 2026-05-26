package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type sequenceExecutor struct {
	mu       sync.Mutex
	execAuth []string
}

func (e *sequenceExecutor) Identifier() string { return "codex" }

func (e *sequenceExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts

	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	e.mu.Lock()
	e.execAuth = append(e.execAuth, authID)
	e.mu.Unlock()

	if authID == "auth-a" {
		return cliproxyexecutor.Response{}, &Error{
			Code:       "upstream_failed",
			Message:    "upstream failed",
			HTTPStatus: http.StatusBadGateway,
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *sequenceExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *sequenceExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *sequenceExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *sequenceExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *sequenceExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.execAuth))
	copy(out, e.execAuth)
	return out
}

type successfulSequenceExecutor struct {
	sequenceExecutor
}

func (e *successfulSequenceExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts

	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	e.mu.Lock()
	e.execAuth = append(e.execAuth, authID)
	e.mu.Unlock()

	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

type successfulProviderExecutor struct {
	sequenceExecutor
	provider string
}

func (e *successfulProviderExecutor) Identifier() string {
	return e.provider
}

type invalidModelExecutor struct {
	sequenceExecutor
}

func (e *invalidModelExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts

	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	e.mu.Lock()
	e.execAuth = append(e.execAuth, authID)
	e.mu.Unlock()

	if authID == "auth-a" {
		return cliproxyexecutor.Response{}, &Error{
			Code:       "invalid_model",
			Message:    `{"detail":"The 'gpt-5.1-codex' model is not supported when using Codex with a ChatGPT account."}`,
			HTTPStatus: http.StatusBadRequest,
		}
	}
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func registerGroupedRouteTestAuths(t *testing.T, manager *Manager) {
	t.Helper()

	for _, auth := range []*Auth{
		{
			ID:       "auth-a",
			Provider: "codex",
			Status:   StatusActive,
			Prefix:   "pro",
			Metadata: map[string]any{"email": "a@example.com"},
		},
		{
			ID:       "auth-b",
			Provider: "codex",
			Status:   StatusActive,
			Prefix:   "pro",
			Metadata: map[string]any{"email": "b@example.com"},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
}

func TestManagerExecute_GroupedRouteFailsOverWithinGroup(t *testing.T) {
	t.Parallel()

	executor := &sequenceExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerGroupedRouteTestAuths(t, manager)

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "pro"},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != "ok" {
		t.Fatalf("Execute() payload = %q, want %q", string(resp.Payload), "ok")
	}

	calls := executor.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream attempts within group, got %v", calls)
	}
	if calls[0] != "auth-a" || calls[1] != "auth-b" {
		t.Fatalf("expected grouped route failover sequence [auth-a auth-b], got %v", calls)
	}
}

func TestManagerExecute_NonGroupedRouteStillFailsOver(t *testing.T) {
	t.Parallel()

	executor := &sequenceExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: false,
		},
	})
	manager.RegisterExecutor(executor)
	registerGroupedRouteTestAuths(t, manager)

	resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(resp.Payload) != "ok" {
		t.Fatalf("Execute() payload = %q, want %q", string(resp.Payload), "ok")
	}

	calls := executor.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 upstream attempts, got %v", calls)
	}
	if calls[0] != "auth-a" || calls[1] != "auth-b" {
		t.Fatalf("expected failover sequence [auth-a auth-b], got %v", calls)
	}
}

func TestManagerExecute_RootRouteUsesDefaultGroupAndExcludesIsolatedGroups(t *testing.T) {
	codexExecutor := &successfulProviderExecutor{provider: "codex"}
	kimiExecutor := &successfulProviderExecutor{provider: "kimi"}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			IncludeDefaultGroup: true,
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:               "kimicode",
					ExcludeFromDefault: true,
					Match: internalconfig.ChannelGroupMatch{
						Channels: []string{"Kimi Channel"},
					},
				},
			},
		},
	})
	manager.RegisterExecutor(codexExecutor)
	manager.RegisterExecutor(kimiExecutor)
	for _, auth := range []*Auth{
		{
			ID:       "codex-default-auth",
			Label:    "Default Codex",
			Provider: "codex",
			Status:   StatusActive,
		},
		{
			ID:       "kimi-isolated-auth",
			Label:    "Kimi Channel",
			Provider: "kimi",
			Status:   StatusActive,
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}

	if _, err := manager.Execute(context.Background(), []string{"kimi", "codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if calls := kimiExecutor.Calls(); len(calls) != 0 {
		t.Fatalf("root default route should not call isolated Kimi group, got %v", calls)
	}
	if calls := codexExecutor.Calls(); len(calls) != 1 || calls[0] != "codex-default-auth" {
		t.Fatalf("root default route should call default Codex auth, got %v", calls)
	}

	if _, err := manager.Execute(context.Background(), []string{"kimi", "codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{
		Metadata: map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "kimicode"},
	}); err != nil {
		t.Fatalf("group Execute() error = %v", err)
	}
	if calls := kimiExecutor.Calls(); len(calls) != 1 || calls[0] != "kimi-isolated-auth" {
		t.Fatalf("explicit group route should call isolated Kimi auth, got %v", calls)
	}
}

func TestManagerExecute_ModelNotSupportedBadRequestDoesNotFailOver(t *testing.T) {
	t.Parallel()

	executor := &invalidModelExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerGroupedRouteTestAuths(t, manager)

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected error")
	}

	calls := executor.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 upstream attempt, got %v", calls)
	}
	if calls[0] != "auth-a" {
		t.Fatalf("expected first auth only, got %v", calls)
	}
}

func TestManagerExecute_GroupStrategyFillFirstOverridesGlobalRoundRobin(t *testing.T) {
	t.Parallel()

	executor := &successfulSequenceExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:     "pro",
					Strategy: "fill-first",
					Match: internalconfig.ChannelGroupMatch{
						Prefixes: []string{"pro"},
					},
				},
			},
		},
	})
	manager.RegisterExecutor(executor)
	registerGroupedRouteTestAuths(t, manager)

	for i := 0; i < 2; i++ {
		resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "pro"},
		})
		if err != nil {
			t.Fatalf("Execute(%d) error = %v", i, err)
		}
		if string(resp.Payload) != "ok" {
			t.Fatalf("Execute(%d) payload = %q, want ok", i, string(resp.Payload))
		}
	}

	if calls := executor.Calls(); len(calls) != 2 || calls[0] != "auth-a" || calls[1] != "auth-a" {
		t.Fatalf("group fill-first should keep using first available auth, got %v", calls)
	}
}

func TestManagerExecute_GroupStrategyRoundRobinOverridesGlobalFillFirst(t *testing.T) {
	t.Parallel()

	executor := &successfulSequenceExecutor{}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetConfig(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "fill-first",
			ChannelGroups: []internalconfig.RoutingChannelGroup{
				{
					Name:     "pro",
					Strategy: "round-robin",
					Match: internalconfig.ChannelGroupMatch{
						Prefixes: []string{"pro"},
					},
				},
			},
		},
	})
	manager.RegisterExecutor(executor)
	registerGroupedRouteTestAuths(t, manager)

	for i := 0; i < 2; i++ {
		resp, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{
			Metadata: map[string]any{cliproxyexecutor.RouteGroupMetadataKey: "pro"},
		})
		if err != nil {
			t.Fatalf("Execute(%d) error = %v", i, err)
		}
		if string(resp.Payload) != "ok" {
			t.Fatalf("Execute(%d) payload = %q, want ok", i, string(resp.Payload))
		}
	}

	if calls := executor.Calls(); len(calls) != 2 || calls[0] != "auth-a" || calls[1] != "auth-b" {
		t.Fatalf("group round-robin should rotate scoped route auths, got %v", calls)
	}
}

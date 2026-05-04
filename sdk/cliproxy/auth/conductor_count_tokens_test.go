package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type countTokensFailureExecutor struct{}

func (e *countTokensFailureExecutor) Identifier() string { return "claude" }

func (e *countTokensFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *countTokensFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, &Error{Code: "not_implemented", Message: "ExecuteStream not implemented"}
}

func (e *countTokensFailureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *countTokensFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &Error{
		Code:       "unsupported_count_tokens",
		Message:    "count_tokens is not supported by this upstream",
		HTTPStatus: http.StatusNotFound,
	}
}

func (e *countTokensFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func TestExecuteCountFailureDoesNotPoisonMessageAvailability(t *testing.T) {
	t.Parallel()

	const (
		authID = "count-token-failure-auth"
		model  = "mimo-v2.5-pro"
	)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model, Created: time.Now().Unix()}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})

	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&countTokensFailureExecutor{})
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "claude",
		Status:   StatusActive,
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	_, err := manager.ExecuteCount(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected count_tokens error")
	}

	resp, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() after count_tokens failure error = %v", err)
	}
	if string(resp.Payload) != `{"ok":true}` {
		t.Fatalf("Execute() payload = %q", string(resp.Payload))
	}
}

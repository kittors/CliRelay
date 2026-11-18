package iflow

import (
	"context"
	"errors"
	"testing"
	"time"

	internaliflow "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type iflowOAuthAuthStub struct {
	exchangeErr error
}

func (s iflowOAuthAuthStub) AuthorizationURL(state string, port int) (string, string) {
	return "https://example.com/iflow?state=" + state, "http://localhost:11451/oauth2callback"
}

func (s iflowOAuthAuthStub) ExchangeCodeForTokens(_ context.Context, code, redirectURI string) (*internaliflow.IFlowTokenData, error) {
	if code != "code" || redirectURI != "http://localhost:11451/oauth2callback" {
		return nil, errors.New("unexpected exchange inputs")
	}
	if s.exchangeErr != nil {
		return nil, s.exchangeErr
	}
	return &internaliflow.IFlowTokenData{
		AccessToken: "access",
		APIKey:      "api-key",
		Email:       "user@example.com",
	}, nil
}

func (s iflowOAuthAuthStub) CreateTokenStorage(tokenData *internaliflow.IFlowTokenData) *internaliflow.IFlowTokenStorage {
	return &internaliflow.IFlowTokenStorage{
		AccessToken: tokenData.AccessToken,
		APIKey:      tokenData.APIKey,
		Email:       tokenData.Email,
		Type:        "iflow",
	}
}

func TestStartOAuthLoginRegistersStartsForwarderAndCompletesProvider(t *testing.T) {
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var startedTarget string
	var savedProvider string
	var completedProvider string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  iflowOAuthAuthStub{},
		WebUI: true,
		Now: func() time.Time {
			return time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
		},
		State: func() string { return "iflow-state" },
		CallbackTarget: func(path string) (string, error) {
			if path != "/iflow/callback" {
				t.Fatalf("callback path = %q, want /iflow/callback", path)
			}
			return "http://127.0.0.1:8080/iflow/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, error) {
			if port != internaliflow.CallbackPort || provider != "iflow" {
				t.Fatalf("forwarder = %d/%s, want iflow callback port", port, provider)
			}
			startedTarget = targetBase
			return "forwarder", nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "iflow" || state != "iflow-state" {
				t.Fatalf("wait callback = %s/%s, want iflow/iflow-state", provider, state)
			}
			if timeout != oauthsession.DefaultTTL {
				t.Fatalf("timeout = %s, want default ttl", timeout)
			}
			return map[string]string{"state": state, "code": "code"}, nil
		},
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registered = state + ":" + provider
			},
			Complete: func(state string) {
				if state != "iflow-state" {
					t.Errorf("complete state = %q, want iflow-state", state)
				}
			},
			CompleteProvider: func(provider string) int {
				completedProvider = provider
				close(completed)
				return 1
			},
		},
		SaveRecord: func(ctx context.Context, record *coreauth.Auth) (string, error) {
			_ = ctx
			if record == nil {
				t.Fatal("record is nil")
			}
			savedProvider = record.Provider
			return "/tmp/iflow.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/iflow?state=iflow-state" || result.State != "iflow-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if registered != "iflow-state:iflow" {
		t.Fatalf("registered = %q, want iflow-state:iflow", registered)
	}
	if startedTarget != "http://127.0.0.1:8080/iflow/callback" {
		t.Fatalf("started target = %q, want callback target", startedTarget)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for provider completion")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for forwarder stop")
	}
	if savedProvider != "iflow" {
		t.Fatalf("saved provider = %q, want iflow", savedProvider)
	}
	if completedProvider != "iflow" {
		t.Fatalf("completed provider = %q, want iflow", completedProvider)
	}
}

func TestStartOAuthLoginSetsErrorOnCallbackError(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  iflowOAuthAuthStub{},
		State: func() string { return "iflow-state" },
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return map[string]string{"state": "iflow-state", "error": "denied"}, nil
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "iflow-state" {
					t.Errorf("error state = %q, want iflow-state", state)
				}
				status = message
				close(done)
			},
		},
		SaveRecord: func(context.Context, *coreauth.Auth) (string, error) {
			t.Fatal("SaveRecord should not be called")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error")
	}
	if status != "Authentication failed" {
		t.Fatalf("status = %q, want Authentication failed", status)
	}
}

func TestStartOAuthLoginIgnoresNonPendingSession(t *testing.T) {
	done := make(chan struct{})

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  iflowOAuthAuthStub{},
		State: func() string { return "iflow-state" },
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			close(done)
			return nil, oauthsession.ErrNotPending
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				t.Fatalf("SetError(%q, %q) should not be called", state, message)
			},
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for callback wait")
	}
}

func TestStartOAuthLoginReturnsCallbackTargetError(t *testing.T) {
	wantErr := errors.New("missing port")
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  iflowOAuthAuthStub{},
		WebUI: true,
		CallbackTarget: func(string) (string, error) {
			return "", wantErr
		},
	})
	if !errors.Is(err, ErrCallbackUnavailable) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback unavailable", err)
	}
}

func TestStartOAuthLoginReturnsForwarderStartError(t *testing.T) {
	wantErr := errors.New("port busy")
	var registered string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  iflowOAuthAuthStub{},
		WebUI: true,
		State: func() string { return "iflow-state" },
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8080/iflow/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, error) {
			return nil, wantErr
		},
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registered = state + ":" + provider
			},
		},
	})
	if !errors.Is(err, ErrCallbackStart) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback start", err)
	}
	if registered != "iflow-state:iflow" {
		t.Fatalf("registered = %q, want session registered before callback start failure", registered)
	}
}

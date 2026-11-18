package geminicli

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	geminiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/oauth2"
)

func TestStartOAuthLoginStartsForwarderAndCompletesProvider(t *testing.T) {
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var savedProvider string
	var savedProjectID string
	var completedProvider string
	var authRedirect string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config:    &config.Config{},
		WebUI:     true,
		ProjectID: "project-1",
		GenerateState: func() (string, error) {
			return "gemini-state", nil
		},
		CallbackTarget: func(path string) (string, error) {
			if path != "/google/callback" {
				t.Fatalf("callback path = %q, want /google/callback", path)
			}
			return "http://127.0.0.1:8080/google/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, int, error) {
			if port != geminiauth.DefaultCallbackPort || provider != "gemini" {
				t.Fatalf("forwarder = %d/%s, want gemini default port", port, provider)
			}
			if targetBase != "http://127.0.0.1:8080/google/callback" {
				t.Fatalf("target = %q, want callback target", targetBase)
			}
			return "forwarder", 60000, nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		AuthCodeURL: func(conf *oauth2.Config, state string) string {
			authRedirect = conf.RedirectURL
			if state != "gemini-state" {
				t.Fatalf("auth state = %q, want gemini-state", state)
			}
			return "https://example.com/gemini?state=" + state
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "gemini" || state != "gemini-state" {
				t.Fatalf("wait callback = %s/%s, want gemini/gemini-state", provider, state)
			}
			if timeout != oauthsession.DefaultTTL {
				t.Fatalf("timeout = %s, want default ttl", timeout)
			}
			return map[string]string{"code": "code"}, nil
		},
		ExchangeToken: func(ctx context.Context, conf *oauth2.Config, code string) (*oauth2.Token, error) {
			_ = ctx
			if code != "code" || conf.RedirectURL != "http://localhost:60000/oauth2callback" {
				t.Fatalf("exchange = %s/%s, want code and forwarder redirect", code, conf.RedirectURL)
			}
			return &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}, nil
		},
		FetchUserInfo: func(context.Context, *oauth2.Config, *oauth2.Token) (UserInfoResult, error) {
			return UserInfoResult{
				Email: "gemini@example.com",
				TokenMap: map[string]any{
					"access_token": "access",
				},
			}, nil
		},
		AuthenticatedClient: func(context.Context, *geminiauth.GeminiTokenStorage, *config.Config) (*http.Client, error) {
			return &http.Client{}, nil
		},
		ConfigureProject: func(ctx context.Context, client *http.Client, storage *geminiauth.GeminiTokenStorage, requestedProjectID string) error {
			_ = ctx
			_ = client
			if requestedProjectID != "project-1" {
				t.Fatalf("requested project = %q, want project-1", requestedProjectID)
			}
			storage.ProjectID = requestedProjectID
			storage.Checked = true
			return nil
		},
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registered = state + ":" + provider
			},
			Complete: func(state string) {
				if state != "gemini-state" {
					t.Errorf("complete state = %q, want gemini-state", state)
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
			if projectID, _ := record.Metadata["project_id"].(string); projectID != "" {
				savedProjectID = projectID
			}
			return "/tmp/gemini.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/gemini?state=gemini-state" || result.State != "gemini-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if authRedirect != "http://localhost:60000/oauth2callback" {
		t.Fatalf("auth redirect = %q, want forwarder redirect", authRedirect)
	}
	if registered != "gemini-state:gemini" {
		t.Fatalf("registered = %q, want gemini-state:gemini", registered)
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
	if savedProvider != "gemini" {
		t.Fatalf("saved provider = %q, want gemini", savedProvider)
	}
	if savedProjectID != "project-1" {
		t.Fatalf("saved project ID = %q, want project-1", savedProjectID)
	}
	if completedProvider != "gemini" {
		t.Fatalf("completed provider = %q, want gemini", completedProvider)
	}
}

func TestStartOAuthLoginReturnsForwarderStartError(t *testing.T) {
	var registered string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config: &config.Config{},
		WebUI:  true,
		GenerateState: func() (string, error) {
			return "gemini-state", nil
		},
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8080/google/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, int, error) {
			return nil, 0, errors.New("port busy")
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
	if registered != "gemini-state:gemini" {
		t.Fatalf("registered = %q, want session registered before callback start failure", registered)
	}
}

func TestStartOAuthLoginReturnsMissingClientID(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{})
	if !errors.Is(err, ErrOAuthClientIDMissing) {
		t.Fatalf("StartOAuthLogin() error = %v, want missing client ID", err)
	}
}

func TestStartOAuthLoginReturnsStateGenerationError(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config: &config.Config{},
		GenerateState: func() (string, error) {
			return "", errors.New("state failed")
		},
	})
	if !errors.Is(err, ErrStateGeneration) {
		t.Fatalf("StartOAuthLogin() error = %v, want state generation", err)
	}
}

func TestStartOAuthLoginReturnsCallbackUnavailable(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config: &config.Config{},
		WebUI:  true,
		GenerateState: func() (string, error) {
			return "gemini-state", nil
		},
		CallbackTarget: func(string) (string, error) {
			return "", errors.New("no callback target")
		},
	})
	if !errors.Is(err, ErrCallbackUnavailable) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback unavailable", err)
	}
}

func TestStartOAuthLoginSetsTimeoutOnWaitFailure(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config: &config.Config{},
		GenerateState: func() (string, error) {
			return "gemini-state", nil
		},
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, errors.New("timeout")
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "gemini-state" {
					t.Errorf("error state = %q, want gemini-state", state)
				}
				status = message
				close(done)
			},
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for session error")
	}
	if status != "OAuth flow timed out" {
		t.Fatalf("status = %q, want OAuth flow timed out", status)
	}
}

func TestStartOAuthLoginPropagatesProjectSessionError(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Config: &config.Config{},
		GenerateState: func() (string, error) {
			return "gemini-state", nil
		},
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return map[string]string{"code": "code"}, nil
		},
		ExchangeToken: func(context.Context, *oauth2.Config, string) (*oauth2.Token, error) {
			return &oauth2.Token{AccessToken: "access"}, nil
		},
		FetchUserInfo: func(context.Context, *oauth2.Config, *oauth2.Token) (UserInfoResult, error) {
			return UserInfoResult{Email: "gemini@example.com", TokenMap: map[string]any{"access_token": "access"}}, nil
		},
		AuthenticatedClient: func(context.Context, *geminiauth.GeminiTokenStorage, *config.Config) (*http.Client, error) {
			return &http.Client{}, nil
		},
		ConfigureProject: func(context.Context, *http.Client, *geminiauth.GeminiTokenStorage, string) error {
			return &SessionError{Message: "Cloud AI API not enabled"}
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "gemini-state" {
					t.Errorf("error state = %q, want gemini-state", state)
				}
				status = message
				close(done)
			},
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for session error")
	}
	if status != "Cloud AI API not enabled" {
		t.Fatalf("status = %q, want Cloud AI API not enabled", status)
	}
}

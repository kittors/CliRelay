package claude

import (
	"context"
	"errors"
	"testing"
	"time"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type claudeOAuthAuthStub struct {
	authURLErr       error
	exchangeErr      error
	expectedRedirect string
	lastRedirect     string
}

func (s *claudeOAuthAuthStub) GenerateAuthURLWithRedirectURI(state string, pkceCodes *internalclaude.PKCECodes, redirectURI string) (string, string, error) {
	if pkceCodes == nil || pkceCodes.CodeVerifier != "verifier" {
		return "", "", errors.New("unexpected pkce")
	}
	s.lastRedirect = redirectURI
	if s.authURLErr != nil {
		return "", "", s.authURLErr
	}
	return "https://example.com/claude?state=" + state, state, nil
}

func (s *claudeOAuthAuthStub) ExchangeCodeForTokensWithRedirectURI(_ context.Context, code, state string, pkceCodes *internalclaude.PKCECodes, redirectURI string) (*internalclaude.ClaudeAuthBundle, error) {
	if code != "code" || state != "claude-state" || pkceCodes.CodeVerifier != "verifier" {
		return nil, errors.New("unexpected exchange inputs")
	}
	if s.expectedRedirect != "" && redirectURI != s.expectedRedirect {
		return nil, errors.New("redirect mismatch")
	}
	if s.exchangeErr != nil {
		return nil, s.exchangeErr
	}
	return &internalclaude.ClaudeAuthBundle{
		APIKey: "api-key",
		TokenData: internalclaude.ClaudeTokenData{
			AccessToken: "access",
			Email:       "claude@example.com",
		},
	}, nil
}

func (s *claudeOAuthAuthStub) CreateTokenStorage(bundle *internalclaude.ClaudeAuthBundle) *internalclaude.ClaudeTokenStorage {
	return &internalclaude.ClaudeTokenStorage{
		AccessToken: bundle.TokenData.AccessToken,
		Email:       bundle.TokenData.Email,
		Type:        "claude",
	}
}

func TestStartOAuthLoginUsesForwarderRedirectAndCompletesProvider(t *testing.T) {
	auth := &claudeOAuthAuthStub{expectedRedirect: "http://localhost:60000/callback"}
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var savedProvider string
	var completedProvider string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  auth,
		WebUI: true,
		GeneratePKCE: func() (*internalclaude.PKCECodes, error) {
			return &internalclaude.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "claude-state", nil },
		CallbackTarget: func(path string) (string, error) {
			if path != "/anthropic/callback" {
				t.Fatalf("callback path = %q, want /anthropic/callback", path)
			}
			return "http://127.0.0.1:8080/anthropic/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, int, error) {
			if port != defaultCallbackPort || provider != "anthropic" {
				t.Fatalf("forwarder = %d/%s, want anthropic default port", port, provider)
			}
			if targetBase != "http://127.0.0.1:8080/anthropic/callback" {
				t.Fatalf("target = %q, want callback target", targetBase)
			}
			return "forwarder", 60000, nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "anthropic" || state != "claude-state" {
				t.Fatalf("wait callback = %s/%s, want anthropic/claude-state", provider, state)
			}
			if timeout != oauthsession.DefaultTTL {
				t.Fatalf("timeout = %s, want default ttl", timeout)
			}
			return map[string]string{"state": state, "code": "code#fragment"}, nil
		},
		Sessions: SessionCallbacks{
			Register: func(state, provider string) {
				registered = state + ":" + provider
			},
			Complete: func(state string) {
				if state != "claude-state" {
					t.Errorf("complete state = %q, want claude-state", state)
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
			return "/tmp/claude.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/claude?state=claude-state" || result.State != "claude-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if auth.lastRedirect != "http://localhost:60000/callback" {
		t.Fatalf("redirect = %q, want forwarder redirect", auth.lastRedirect)
	}
	if registered != "claude-state:anthropic" {
		t.Fatalf("registered = %q, want claude-state:anthropic", registered)
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
	if savedProvider != "claude" {
		t.Fatalf("saved provider = %q, want claude", savedProvider)
	}
	if completedProvider != "anthropic" {
		t.Fatalf("completed provider = %q, want anthropic", completedProvider)
	}
}

func TestStartOAuthLoginFallsBackToPlatformRedirect(t *testing.T) {
	auth := &claudeOAuthAuthStub{}

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  auth,
		WebUI: true,
		GeneratePKCE: func() (*internalclaude.PKCECodes, error) {
			return &internalclaude.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "claude-state", nil },
		CallbackTarget: func(string) (string, error) {
			return "", errors.New("unavailable")
		},
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, oauthsession.ErrNotPending
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.State != "claude-state" {
		t.Fatalf("state = %q, want claude-state", result.State)
	}
	if auth.lastRedirect != internalclaude.PlatformRedirectURI {
		t.Fatalf("redirect = %q, want platform redirect", auth.lastRedirect)
	}
}

// Code mode never starts a localhost forwarder: the operator copies the code
// from Anthropic's own page, so the redirect must be the platform callback even
// though a callback target and forwarder would be available.
func TestStartOAuthLoginCodeModeUsesPlatformCallbackWithoutForwarder(t *testing.T) {
	auth := &claudeOAuthAuthStub{}
	forwarderStarted := false

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:                auth,
		WebUI:               true,
		UsePlatformCallback: true,
		GeneratePKCE: func() (*internalclaude.PKCECodes, error) {
			return &internalclaude.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "claude-state", nil },
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8317/anthropic/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, int, error) {
			forwarderStarted = true
			return nil, 54545, nil
		},
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, oauthsession.ErrNotPending
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if auth.lastRedirect != internalclaude.PlatformRedirectURI {
		t.Fatalf("redirect = %q, want platform redirect", auth.lastRedirect)
	}
	if forwarderStarted {
		t.Fatal("code mode must not start a localhost callback forwarder")
	}
}

func TestStartOAuthLoginReturnsPKCEError(t *testing.T) {
	wantErr := errors.New("rng")
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		GeneratePKCE: func() (*internalclaude.PKCECodes, error) {
			return nil, wantErr
		},
	})
	if !errors.Is(err, ErrPKCEGeneration) {
		t.Fatalf("StartOAuthLogin() error = %v, want PKCE generation", err)
	}
}

func TestStartOAuthLoginSetsTimeoutOnWaitFailure(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth: &claudeOAuthAuthStub{},
		GeneratePKCE: func() (*internalclaude.PKCECodes, error) {
			return &internalclaude.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "claude-state", nil },
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, errors.New("timeout")
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "claude-state" {
					t.Errorf("error state = %q, want claude-state", state)
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
	if status != "Timeout waiting for OAuth callback" {
		t.Fatalf("status = %q, want timeout message", status)
	}
}

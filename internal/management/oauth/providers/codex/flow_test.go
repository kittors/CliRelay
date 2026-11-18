package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type codexOAuthAuthStub struct {
	authURLErr  error
	exchangeErr error
}

func (s codexOAuthAuthStub) GenerateAuthURL(state string, pkceCodes *internalcodex.PKCECodes) (string, error) {
	if pkceCodes == nil || pkceCodes.CodeVerifier != "verifier" {
		return "", errors.New("unexpected pkce")
	}
	if s.authURLErr != nil {
		return "", s.authURLErr
	}
	return "https://example.com/codex?state=" + state, nil
}

func (s codexOAuthAuthStub) ExchangeCodeForTokens(_ context.Context, code string, pkceCodes *internalcodex.PKCECodes) (*internalcodex.CodexAuthBundle, error) {
	if code != "code" || pkceCodes.CodeVerifier != "verifier" {
		return nil, errors.New("unexpected exchange inputs")
	}
	if s.exchangeErr != nil {
		return nil, s.exchangeErr
	}
	return &internalcodex.CodexAuthBundle{
		APIKey: "api-key",
		TokenData: internalcodex.CodexTokenData{
			AccessToken: "access",
			IDToken:     unsignedJWTForFlow(tClaims{"email": "codex@example.com"}),
		},
	}, nil
}

func (s codexOAuthAuthStub) CreateTokenStorage(bundle *internalcodex.CodexAuthBundle) *internalcodex.CodexTokenStorage {
	return &internalcodex.CodexTokenStorage{
		AccessToken: bundle.TokenData.AccessToken,
		IDToken:     bundle.TokenData.IDToken,
		Email:       "codex@example.com",
		Type:        "codex",
	}
}

func TestStartOAuthLoginStartsForwarderAndCompletesProvider(t *testing.T) {
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var savedProvider string
	var completedProvider string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  codexOAuthAuthStub{},
		WebUI: true,
		GeneratePKCE: func() (*internalcodex.PKCECodes, error) {
			return &internalcodex.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "codex-state", nil },
		CallbackTarget: func(path string) (string, error) {
			if path != "/codex/callback" {
				t.Fatalf("callback path = %q, want /codex/callback", path)
			}
			return "http://127.0.0.1:8080/codex/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, error) {
			if port != defaultCallbackPort || provider != "codex" {
				t.Fatalf("forwarder = %d/%s, want codex default port", port, provider)
			}
			if targetBase != "http://127.0.0.1:8080/codex/callback" {
				t.Fatalf("target = %q, want callback target", targetBase)
			}
			return "forwarder", nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "codex" || state != "codex-state" {
				t.Fatalf("wait callback = %s/%s, want codex/codex-state", provider, state)
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
				if state != "codex-state" {
					t.Errorf("complete state = %q, want codex-state", state)
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
			return "/tmp/codex.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/codex?state=codex-state" || result.State != "codex-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if registered != "codex-state:codex" {
		t.Fatalf("registered = %q, want codex-state:codex", registered)
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
	if savedProvider != "codex" {
		t.Fatalf("saved provider = %q, want codex", savedProvider)
	}
	if completedProvider != "codex" {
		t.Fatalf("completed provider = %q, want codex", completedProvider)
	}
}

func TestStartOAuthLoginContinuesWhenForwarderUnavailable(t *testing.T) {
	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  codexOAuthAuthStub{},
		WebUI: true,
		GeneratePKCE: func() (*internalcodex.PKCECodes, error) {
			return &internalcodex.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "codex-state", nil },
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
	if result.State != "codex-state" {
		t.Fatalf("state = %q, want codex-state", result.State)
	}
}

func TestStartOAuthLoginReturnsPKCEError(t *testing.T) {
	wantErr := errors.New("rng")
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		GeneratePKCE: func() (*internalcodex.PKCECodes, error) {
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
		Auth: codexOAuthAuthStub{},
		GeneratePKCE: func() (*internalcodex.PKCECodes, error) {
			return &internalcodex.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "codex-state", nil },
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, errors.New("timeout")
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "codex-state" {
					t.Errorf("error state = %q, want codex-state", state)
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

type tClaims map[string]any

func unsignedJWTForFlow(claims tClaims) string {
	header := map[string]any{"alg": "none"}
	return encodeJWTPartForFlow(header) + "." + encodeJWTPartForFlow(claims) + "."
}

func encodeJWTPartForFlow(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

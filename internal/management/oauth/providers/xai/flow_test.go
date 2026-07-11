package xai

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type xaiOAuthAuthStub struct {
	discoverErr error
	tokenErr    error
}

func (s xaiOAuthAuthStub) Discover(context.Context) (*internalxai.Discovery, error) {
	if s.discoverErr != nil {
		return nil, s.discoverErr
	}
	return &internalxai.Discovery{
		AuthorizationEndpoint: "https://auth.x.ai/authorize",
		TokenEndpoint:         "https://auth.x.ai/token",
	}, nil
}

func (s xaiOAuthAuthStub) ExchangeCodeForTokens(_ context.Context, code, redirectURI string, pkce *internalxai.PKCECodes, tokenEndpoint string) (*internalxai.AuthBundle, error) {
	if code != "code" || redirectURI != "http://127.0.0.1:60000/callback" || tokenEndpoint != "https://auth.x.ai/token" {
		return nil, errors.New("unexpected exchange inputs")
	}
	if pkce == nil || pkce.CodeVerifier != "verifier" {
		return nil, errors.New("unexpected pkce")
	}
	if s.tokenErr != nil {
		return nil, s.tokenErr
	}
	return &internalxai.AuthBundle{
		TokenData: internalxai.TokenData{
			AccessToken:  "access",
			RefreshToken: "refresh",
			ExpiresIn:    3600,
			Email:        "user@example.com",
			Subject:      "sub-1",
		},
		LastRefresh:   time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC).Format(time.RFC3339),
		BaseURL:       internalxai.DefaultAPIBaseURL,
		RedirectURI:   redirectURI,
		TokenEndpoint: tokenEndpoint,
	}, nil
}

func (s xaiOAuthAuthStub) CreateTokenStorage(bundle *internalxai.AuthBundle) *internalxai.TokenStorage {
	return internalxai.NewXAIAuth(nil).CreateTokenStorage(bundle)
}

func TestStartOAuthLoginStartsForwarderAndCompletesProvider(t *testing.T) {
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var savedProvider string
	var completedProvider string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  xaiOAuthAuthStub{},
		WebUI: true,
		GeneratePKCE: func() (*internalxai.PKCECodes, error) {
			return &internalxai.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) {
			return "xai-state", nil
		},
		GenerateNonce: func() (string, error) {
			return "xai-nonce", nil
		},
		CallbackTarget: func(path string) (string, error) {
			if path != "/xai/callback" {
				t.Fatalf("callback path = %q, want /xai/callback", path)
			}
			return "http://127.0.0.1:8080/xai/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, int, error) {
			if port != internalxai.CallbackPort || provider != "xai" {
				t.Fatalf("forwarder = %d/%s, want xai callback port", port, provider)
			}
			if targetBase != "http://127.0.0.1:8080/xai/callback" {
				t.Fatalf("target = %q, want callback target", targetBase)
			}
			return "forwarder", 60000, nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "xai" || state != "xai-state" {
				t.Fatalf("wait callback = %s/%s, want xai/xai-state", provider, state)
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
			if got := record.Attributes["using_api"]; got != "false" {
				t.Fatalf("attributes[using_api] = %q, want false", got)
			}
			if got, ok := record.Metadata["using_api"].(bool); !ok || got {
				t.Fatalf("metadata[using_api] = %#v, want false", record.Metadata["using_api"])
			}
			savedProvider = record.Provider
			return "/tmp/xai.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.State != "xai-state" {
		t.Fatalf("state = %q, want xai-state", result.State)
	}
	authURL, errParse := url.Parse(result.AuthURL)
	if errParse != nil {
		t.Fatalf("auth url parse failed: %v", errParse)
	}
	if authURL.Hostname() != "auth.x.ai" || !strings.Contains(authURL.RawQuery, "code_challenge=challenge") {
		t.Fatalf("auth url = %q, want xAI authorize URL with challenge", result.AuthURL)
	}
	if got := authURL.Query().Get("redirect_uri"); got != "http://127.0.0.1:60000/callback" {
		t.Fatalf("redirect_uri = %q, want actual forwarder port", got)
	}
	if registered != "xai-state:xai" {
		t.Fatalf("registered = %q, want xai-state:xai", registered)
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
	if savedProvider != "xai" || completedProvider != "xai" {
		t.Fatalf("saved/completed provider = %q/%q, want xai/xai", savedProvider, completedProvider)
	}
}

func TestStartOAuthLoginReturnsForwarderStartError(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  xaiOAuthAuthStub{},
		WebUI: true,
		GeneratePKCE: func() (*internalxai.PKCECodes, error) {
			return &internalxai.PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}, nil
		},
		GenerateState: func() (string, error) { return "xai-state", nil },
		GenerateNonce: func() (string, error) { return "xai-nonce", nil },
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8080/xai/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, int, error) {
			return nil, 0, errors.New("port busy")
		},
	})
	if !errors.Is(err, ErrCallbackStart) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback start", err)
	}
}

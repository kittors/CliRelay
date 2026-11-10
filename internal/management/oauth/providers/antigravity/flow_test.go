package antigravity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type antigravityOAuthAuthStub struct {
	authURLEmpty bool
	tokenErr     error
	userInfoErr  error
	projectErr   error
	noProject    bool
}

func (s antigravityOAuthAuthStub) BuildAuthURL(state, redirectURI string) string {
	if state != "antigravity-state" || redirectURI != "http://localhost:60000/oauth-callback" {
		return ""
	}
	if s.authURLEmpty {
		return ""
	}
	return "https://example.com/antigravity?state=" + state
}

func (s antigravityOAuthAuthStub) ExchangeCodeForTokens(_ context.Context, code, redirectURI string) (*internalantigravity.TokenResponse, error) {
	if code != "code" || redirectURI != "http://localhost:60000/oauth-callback" {
		return nil, errors.New("unexpected exchange inputs")
	}
	if s.tokenErr != nil {
		return nil, s.tokenErr
	}
	return &internalantigravity.TokenResponse{
		AccessToken:  "access",
		RefreshToken: "refresh",
		ExpiresIn:    3600,
	}, nil
}

func (s antigravityOAuthAuthStub) FetchUserInfo(_ context.Context, accessToken string) (string, error) {
	if accessToken != "access" {
		return "", errors.New("unexpected access token")
	}
	if s.userInfoErr != nil {
		return "", s.userInfoErr
	}
	return "user@example.com", nil
}

func (s antigravityOAuthAuthStub) FetchProjectID(_ context.Context, accessToken string) (string, error) {
	if accessToken != "access" {
		return "", errors.New("unexpected access token")
	}
	if s.projectErr != nil {
		return "", s.projectErr
	}
	if s.noProject {
		return "", nil
	}
	return "project-1", nil
}

func TestStartOAuthLoginStartsForwarderAndCompletesProvider(t *testing.T) {
	completed := make(chan struct{})
	stopped := make(chan struct{})
	var registered string
	var savedProvider string
	var savedProjectID string
	var completedProvider string

	result, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  antigravityOAuthAuthStub{},
		WebUI: true,
		GenerateState: func() (string, error) {
			return "antigravity-state", nil
		},
		CallbackTarget: func(path string) (string, error) {
			if path != "/antigravity/callback" {
				t.Fatalf("callback path = %q, want /antigravity/callback", path)
			}
			return "http://127.0.0.1:8080/antigravity/callback", nil
		},
		StartForwarder: func(port int, provider, targetBase string) (CallbackForwarder, int, error) {
			if port != internalantigravity.CallbackPort || provider != "antigravity" {
				t.Fatalf("forwarder = %d/%s, want antigravity callback port", port, provider)
			}
			if targetBase != "http://127.0.0.1:8080/antigravity/callback" {
				t.Fatalf("target = %q, want callback target", targetBase)
			}
			return "forwarder", 60000, nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
		WaitCallback: func(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
			if provider != "antigravity" || state != "antigravity-state" {
				t.Fatalf("wait callback = %s/%s, want antigravity/antigravity-state", provider, state)
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
				if state != "antigravity-state" {
					t.Errorf("complete state = %q, want antigravity-state", state)
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
			if projectID, _ := record.Metadata["project_id"].(string); strings.TrimSpace(projectID) != "" {
				savedProjectID = projectID
			}
			return "/tmp/antigravity.json", nil
		},
	})
	if err != nil {
		t.Fatalf("StartOAuthLogin() error = %v", err)
	}
	if result.AuthURL != "https://example.com/antigravity?state=antigravity-state" || result.State != "antigravity-state" {
		t.Fatalf("result = %#v, want auth url and state", result)
	}
	if registered != "antigravity-state:antigravity" {
		t.Fatalf("registered = %q, want antigravity-state:antigravity", registered)
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
	if savedProvider != "antigravity" {
		t.Fatalf("saved provider = %q, want antigravity", savedProvider)
	}
	if savedProjectID != "project-1" {
		t.Fatalf("saved project ID = %q, want project-1", savedProjectID)
	}
	if completedProvider != "antigravity" {
		t.Fatalf("completed provider = %q, want antigravity", completedProvider)
	}
}

func TestStartOAuthLoginReturnsCallbackTargetError(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  antigravityOAuthAuthStub{},
		WebUI: true,
		GenerateState: func() (string, error) {
			return "antigravity-state", nil
		},
		CallbackTarget: func(string) (string, error) {
			return "", errors.New("missing port")
		},
	})
	if !errors.Is(err, ErrCallbackUnavailable) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback unavailable", err)
	}
}

func TestStartOAuthLoginReturnsForwarderStartError(t *testing.T) {
	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  antigravityOAuthAuthStub{},
		WebUI: true,
		GenerateState: func() (string, error) {
			return "antigravity-state", nil
		},
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8080/antigravity/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, int, error) {
			return nil, 0, errors.New("port busy")
		},
	})
	if !errors.Is(err, ErrCallbackStart) {
		t.Fatalf("StartOAuthLogin() error = %v, want callback start", err)
	}
}

func TestStartOAuthLoginStopsForwarderWhenClientIDMissing(t *testing.T) {
	stopped := make(chan struct{})

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth:  antigravityOAuthAuthStub{authURLEmpty: true},
		WebUI: true,
		GenerateState: func() (string, error) {
			return "antigravity-state", nil
		},
		CallbackTarget: func(string) (string, error) {
			return "http://127.0.0.1:8080/antigravity/callback", nil
		},
		StartForwarder: func(int, string, string) (CallbackForwarder, int, error) {
			return "forwarder", 60000, nil
		},
		StopForwarder: func(context.Context, int, CallbackForwarder) {
			close(stopped)
		},
	})
	if !errors.Is(err, ErrOAuthClientIDMissing) {
		t.Fatalf("StartOAuthLogin() error = %v, want missing client ID", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for forwarder stop")
	}
}

func TestStartOAuthLoginSetsTimeoutOnWaitFailure(t *testing.T) {
	done := make(chan struct{})
	var status string

	_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
		Auth: antigravityOAuthAuthStub{},
		GenerateState: func() (string, error) {
			return "antigravity-state", nil
		},
		CallbackPort: 60000,
		WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
			return nil, errors.New("timeout")
		},
		Sessions: SessionCallbacks{
			SetError: func(state, message string) {
				if state != "antigravity-state" {
					t.Errorf("error state = %q, want antigravity-state", state)
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

// A login that cannot find the account's project must fail and say why, not
// save the account and report success: that credential then sent requests
// with a made-up project and drew 403 SUBSCRIPTION_REQUIRED (#1032).
func TestStartOAuthLoginFailsWithoutProject(t *testing.T) {
	cases := []struct {
		name string
		auth antigravityOAuthAuthStub
		want string
	}{
		{
			name: "client refused",
			auth: antigravityOAuthAuthStub{projectErr: fmt.Errorf("%w: This client is not supported.", internalantigravity.ErrUnsupportedClient)},
			want: "Google rejected this client while looking up the Antigravity project",
		},
		{
			name: "onboarding unfinished",
			auth: antigravityOAuthAuthStub{projectErr: fmt.Errorf("%w after 5 polls", internalantigravity.ErrOnboardingPending)},
			want: "log in again shortly",
		},
		{
			name: "empty project",
			auth: antigravityOAuthAuthStub{noProject: true},
			want: "Google returned no Antigravity project for this account",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan string, 1)
			saved := false
			completed := false
			_, err := StartOAuthLogin(context.Background(), OAuthLoginOptions{
				Auth:          tc.auth,
				GenerateState: func() (string, error) { return "antigravity-state", nil },
				CallbackPort:  60000,
				WaitCallback: func(string, string, string, time.Duration) (map[string]string, error) {
					return map[string]string{"state": "antigravity-state", "code": "code"}, nil
				},
				Sessions: SessionCallbacks{
					SetError: func(_, message string) { done <- message },
					Complete: func(string) { completed = true },
				},
				SaveRecord: func(context.Context, *coreauth.Auth) (string, error) {
					saved = true
					return "/tmp/antigravity.json", nil
				},
			})
			if err != nil {
				t.Fatalf("StartOAuthLogin() error = %v", err)
			}
			var message string
			select {
			case message = <-done:
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for the session error")
			}
			if !strings.Contains(message, tc.want) {
				t.Fatalf("session error = %q, want it to contain %q", message, tc.want)
			}
			if saved || completed {
				t.Fatalf("saved = %v, completed = %v: an account without a project must not be saved or reported as logged in", saved, completed)
			}
		})
	}
}

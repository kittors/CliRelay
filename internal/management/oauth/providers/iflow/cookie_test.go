package iflow

import (
	"context"
	"errors"
	"testing"
	"time"

	internaliflow "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/iflow"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type iflowCookieAuthStub struct {
	authErr error
	email   string
}

func (s iflowCookieAuthStub) AuthenticateWithCookie(_ context.Context, cookie string) (*internaliflow.IFlowTokenData, error) {
	if cookie != "BXAuth=cookie;" {
		return nil, errors.New("unexpected cookie")
	}
	if s.authErr != nil {
		return nil, s.authErr
	}
	email := s.email
	if email == "" {
		email = "user@example.com"
	}
	return &internaliflow.IFlowTokenData{
		APIKey: "api-key",
		Email:  email,
		Expire: "2026-06-07T00:00:00Z",
		Cookie: cookie,
	}, nil
}

func (s iflowCookieAuthStub) CreateCookieTokenStorage(tokenData *internaliflow.IFlowTokenData) *internaliflow.IFlowTokenStorage {
	return &internaliflow.IFlowTokenStorage{
		APIKey: tokenData.APIKey,
		Email:  tokenData.Email,
		Expire: tokenData.Expire,
		Cookie: tokenData.Cookie,
		Type:   "iflow",
	}
}

func TestAuthenticateCookieSavesRecord(t *testing.T) {
	var checkedBXAuth string
	var savedProvider string

	result, err := AuthenticateCookie(context.Background(), " BXAuth=cookie ", CookieLoginOptions{
		Auth: iflowCookieAuthStub{},
		Now: func() time.Time {
			return time.Date(2026, 6, 6, 12, 30, 45, 0, time.UTC)
		},
		CheckDuplicate: func(authDir, bxAuth string) (string, error) {
			if authDir != "/auths" {
				t.Fatalf("auth dir = %q, want /auths", authDir)
			}
			checkedBXAuth = bxAuth
			return "", nil
		},
		AuthDir: "/auths",
		SaveRecord: func(ctx context.Context, record *coreauth.Auth) (string, error) {
			_ = ctx
			if record == nil {
				t.Fatal("record is nil")
			}
			savedProvider = record.Provider
			return "/tmp/iflow-cookie.json", nil
		},
	})
	if err != nil {
		t.Fatalf("AuthenticateCookie() error = %v", err)
	}
	if checkedBXAuth != "cookie" {
		t.Fatalf("checked BXAuth = %q, want cookie", checkedBXAuth)
	}
	if savedProvider != "iflow" {
		t.Fatalf("saved provider = %q, want iflow", savedProvider)
	}
	if result.SavedPath != "/tmp/iflow-cookie.json" || result.Email != "user@example.com" || result.Expired != "2026-06-07T00:00:00Z" || result.Type != "iflow" {
		t.Fatalf("result = %#v, want saved cookie result", result)
	}
}

func TestAuthenticateCookieRequiresCookie(t *testing.T) {
	_, err := AuthenticateCookie(context.Background(), " ", CookieLoginOptions{})
	if !errors.Is(err, ErrCookieRequired) {
		t.Fatalf("AuthenticateCookie() error = %v, want cookie required", err)
	}
}

func TestAuthenticateCookieReturnsNormalizeError(t *testing.T) {
	_, err := AuthenticateCookie(context.Background(), "session=value", CookieLoginOptions{})
	if err == nil || err.Error() != "cookie missing BXAuth field" {
		t.Fatalf("AuthenticateCookie() error = %v, want missing BXAuth", err)
	}
}

func TestAuthenticateCookieReturnsDuplicateBXAuth(t *testing.T) {
	_, err := AuthenticateCookie(context.Background(), "BXAuth=cookie;", CookieLoginOptions{
		CheckDuplicate: func(string, string) (string, error) {
			return "/auths/iflow-existing.json", nil
		},
	})
	var duplicate DuplicateBXAuthError
	if !errors.As(err, &duplicate) {
		t.Fatalf("AuthenticateCookie() error = %v, want duplicate BXAuth", err)
	}
	if duplicate.ExistingFileName() != "iflow-existing.json" {
		t.Fatalf("existing file name = %q, want iflow-existing.json", duplicate.ExistingFileName())
	}
}

func TestAuthenticateCookieReturnsAuthError(t *testing.T) {
	wantErr := errors.New("invalid cookie")
	_, err := AuthenticateCookie(context.Background(), "BXAuth=cookie;", CookieLoginOptions{
		Auth: iflowCookieAuthStub{authErr: wantErr},
		CheckDuplicate: func(string, string) (string, error) {
			return "", nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AuthenticateCookie() error = %v, want auth error", err)
	}
}

func TestAuthenticateCookieReturnsSaveError(t *testing.T) {
	wantErr := errors.New("disk full")
	_, err := AuthenticateCookie(context.Background(), "BXAuth=cookie;", CookieLoginOptions{
		Auth: iflowCookieAuthStub{},
		CheckDuplicate: func(string, string) (string, error) {
			return "", nil
		},
		SaveRecord: func(context.Context, *coreauth.Auth) (string, error) {
			return "", wantErr
		},
	})
	if !errors.Is(err, ErrSaveTokens) {
		t.Fatalf("AuthenticateCookie() error = %v, want save tokens error", err)
	}
}

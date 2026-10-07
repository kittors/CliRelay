package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
)

type fakeRefresher struct {
	seen string
	data *internalcodex.CodexTokenData
	err  error
}

func (f *fakeRefresher) RefreshTokens(_ context.Context, refreshToken string) (*internalcodex.CodexTokenData, error) {
	f.seen = refreshToken
	return f.data, f.err
}

// unsignedIDToken builds a JWT-shaped token with the claims the parser reads;
// nothing here is a real credential.
func unsignedIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".sig"
}

func TestImportRefreshTokenStoresTheRotatedToken(t *testing.T) {
	idToken := unsignedIDToken(t, map[string]any{
		"email": "ada@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_account_id": "acct-1",
			"chatgpt_user_id":    "user-1",
		},
	})
	refresher := &fakeRefresher{data: &internalcodex.CodexTokenData{
		IDToken:      idToken,
		AccessToken:  "at-new",
		RefreshToken: "rt-rotated",
		AccountID:    "acct-1",
		Email:        "ada@example.com",
		Expire:       "2026-10-16T00:00:00Z",
	}}
	now := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

	record, details, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
		RefreshToken: "  rt-original  ",
		Refresher:    refresher,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ImportRefreshToken: %v", err)
	}
	if refresher.seen != "rt-original" {
		t.Fatalf("refreshed with %q, want the trimmed input", refresher.seen)
	}
	storage, ok := record.Storage.(*internalcodex.CodexTokenStorage)
	if !ok {
		t.Fatalf("storage = %T", record.Storage)
	}
	if storage.RefreshToken != "rt-rotated" || storage.AccessToken != "at-new" || storage.LastRefresh != now.Format(time.RFC3339) {
		t.Fatalf("storage = %+v; the rotated refresh token must be the one stored", storage)
	}
	if record.Provider != "codex" || !strings.Contains(record.FileName, "ada@example.com") {
		t.Fatalf("record = %s %s", record.Provider, record.FileName)
	}
	if record.Metadata["email"] != "ada@example.com" || record.Metadata["plan_type"] != "pro" {
		t.Fatalf("metadata = %+v", record.Metadata)
	}
	if details != (credentialimport.Details{Email: "ada@example.com", Plan: "pro"}) {
		t.Fatalf("details = %+v", details)
	}
}

func TestImportRefreshTokenClassifiesFailures(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr error
	}{
		{"reused token", errors.New(`token refresh failed with status 400: {"error":"refresh_token_reused"}`), credentialimport.ErrInvalidCredential},
		{"invalid grant", errors.New(`token refresh failed with status 400: {"error":"invalid_grant"}`), credentialimport.ErrInvalidCredential},
		{"unauthorized", errors.New(`token refresh failed with status 401: {}`), credentialimport.ErrInvalidCredential},
		{"cloudflare", errors.New(`token refresh failed with status 403: <!DOCTYPE html><title>Just a moment...</title>`), credentialimport.ErrBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
				RefreshToken: "rt-original",
				Refresher:    &fakeRefresher{err: tc.err},
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	network := errors.New("token refresh request failed: dial tcp: i/o timeout")
	_, _, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
		RefreshToken: "rt-original",
		Refresher:    &fakeRefresher{err: network},
	})
	if errors.Is(err, credentialimport.ErrInvalidCredential) || errors.Is(err, credentialimport.ErrBlocked) {
		t.Fatalf("a network failure is not the credential's fault: %v", err)
	}

	if _, _, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{RefreshToken: "  "}); !errors.Is(err, credentialimport.ErrInvalidCredential) {
		t.Fatalf("blank token: error = %v", err)
	}
}

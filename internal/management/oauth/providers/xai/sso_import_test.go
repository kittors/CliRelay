package xai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
)

type fakeSSOConverter struct {
	seen string
	err  error
}

func (f *fakeSSOConverter) ConvertSSOToTokens(_ context.Context, ssoToken string) (*internalxai.TokenData, string, error) {
	f.seen = ssoToken
	if f.err != nil {
		return nil, "", f.err
	}
	return &internalxai.TokenData{
		AccessToken:  "xai-at-1",
		RefreshToken: "xai-rt-1",
		TokenType:    "Bearer",
		ExpiresIn:    21600,
		Email:        "linus@example.net",
		Subject:      "user-sub-1",
	}, "https://auth.x.ai/oauth2/token", nil
}

func (f *fakeSSOConverter) CreateTokenStorage(bundle *internalxai.AuthBundle) *internalxai.TokenStorage {
	return (&internalxai.XAIAuth{}).CreateTokenStorage(bundle)
}

func TestImportSSOBuildsTheBrowserLoginAccount(t *testing.T) {
	converter := &fakeSSOConverter{}
	now := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	record, details, err := ImportSSO(context.Background(), SSOImportOptions{
		SSOToken:  "fake-sso",
		UsingAPI:  true,
		Converter: converter,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ImportSSO: %v", err)
	}
	if converter.seen != "fake-sso" {
		t.Fatalf("converted %q", converter.seen)
	}
	if record.Provider != "xai" || record.Label != "linus@example.net" {
		t.Fatalf("record = %s %q", record.Provider, record.Label)
	}
	if record.Metadata["refresh_token"] != "xai-rt-1" || record.Metadata[xaiUsingAPIKey] != true {
		t.Fatalf("metadata = %+v", record.Metadata)
	}
	storage, ok := record.Storage.(*internalxai.TokenStorage)
	if !ok || storage.TokenEndpoint != "https://auth.x.ai/oauth2/token" || storage.AuthKind != "oauth" {
		t.Fatalf("storage = %+v; later refreshes need the token endpoint", record.Storage)
	}
	if details.Email != "linus@example.net" {
		t.Fatalf("details = %+v", details)
	}
}

func TestImportSSOMapsFailures(t *testing.T) {
	cases := map[error]error{
		internalxai.ErrSSOInvalid: credentialimport.ErrInvalidCredential,
		fmt.Errorf("%w: approval ended with status 403", internalxai.ErrSSODenied): credentialimport.ErrDenied,
	}
	for upstream, want := range cases {
		_, _, err := ImportSSO(context.Background(), SSOImportOptions{
			SSOToken:  "fake-sso",
			Converter: &fakeSSOConverter{err: upstream},
		})
		if !errors.Is(err, want) {
			t.Errorf("%v: error = %v, want %v", upstream, err, want)
		}
	}
}

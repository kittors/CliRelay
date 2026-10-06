package claude

import (
	"context"
	"errors"
	"fmt"
	"testing"

	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
)

type fakeSessionAuthorizer struct {
	seen string
	err  error
}

func (f *fakeSessionAuthorizer) AuthorizeWithSessionKey(_ context.Context, sessionKey string) (*internalclaude.ClaudeAuthBundle, internalclaude.SessionOrganization, error) {
	f.seen = sessionKey
	if f.err != nil {
		return nil, internalclaude.SessionOrganization{}, f.err
	}
	return &internalclaude.ClaudeAuthBundle{TokenData: internalclaude.ClaudeTokenData{
			AccessToken:  "at-1",
			RefreshToken: "rt-1",
			Email:        "grace@example.com",
			Expire:       "2026-10-06T09:00:00Z",
		}},
		internalclaude.SessionOrganization{UUID: "org-1", Name: "Acme Team"}, nil
}

func (f *fakeSessionAuthorizer) CreateTokenStorage(bundle *internalclaude.ClaudeAuthBundle) *internalclaude.ClaudeTokenStorage {
	return (&internalclaude.ClaudeAuth{}).CreateTokenStorage(bundle)
}

func TestImportSessionKeyBuildsTheBrowserLoginAccount(t *testing.T) {
	authorizer := &fakeSessionAuthorizer{}
	record, details, err := ImportSessionKey(context.Background(), SessionImportOptions{
		SessionKey: "sk-ant-sid01-fake",
		Authorizer: authorizer,
	})
	if err != nil {
		t.Fatalf("ImportSessionKey: %v", err)
	}
	if authorizer.seen != "sk-ant-sid01-fake" {
		t.Fatalf("authorized %q", authorizer.seen)
	}
	if record.Provider != "claude" || record.FileName != CredentialFileName("grace@example.com") {
		t.Fatalf("record = %s %s", record.Provider, record.FileName)
	}
	if record.Metadata["email"] != "grace@example.com" || record.Metadata["access_token"] != "at-1" {
		t.Fatalf("metadata = %+v", record.Metadata)
	}
	if details != (credentialimport.Details{Email: "grace@example.com", Organization: "Acme Team"}) {
		t.Fatalf("details = %+v", details)
	}
}

func TestImportSessionKeyMapsFailures(t *testing.T) {
	cases := map[error]error{
		fmt.Errorf("%w (status 403)", internalclaude.ErrSessionKeyInvalid): credentialimport.ErrInvalidCredential,
		fmt.Errorf("%w (status 403)", internalclaude.ErrSessionBlocked):    credentialimport.ErrBlocked,
		internalclaude.ErrNoOrganization:                                   credentialimport.ErrNoOrganization,
	}
	for upstream, want := range cases {
		_, _, err := ImportSessionKey(context.Background(), SessionImportOptions{
			SessionKey: "sk-ant-sid01-fake",
			Authorizer: &fakeSessionAuthorizer{err: upstream},
		})
		if !errors.Is(err, want) {
			t.Errorf("%v: error = %v, want %v", upstream, err, want)
		}
	}
}

package antigravity

import (
	"context"
	"errors"
	"testing"
	"time"

	internalantigravity "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
)

type fakeRefreshClient struct {
	refreshErr error
	projectErr error
	refreshed  string
}

func (f *fakeRefreshClient) RefreshAccessToken(_ context.Context, refreshToken string) (*internalantigravity.TokenResponse, error) {
	f.refreshed = refreshToken
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return &internalantigravity.TokenResponse{AccessToken: "ya29.fake", ExpiresIn: 3599, TokenType: "Bearer"}, nil
}

func (f *fakeRefreshClient) FetchUserInfo(context.Context, string) (string, error) {
	return "katherine@example.dev", nil
}

func (f *fakeRefreshClient) FetchProjectID(context.Context, string) (string, error) {
	if f.projectErr != nil {
		return "", f.projectErr
	}
	return "project-fake-1", nil
}

func TestImportRefreshTokenKeepsTheStableRefreshToken(t *testing.T) {
	client := &fakeRefreshClient{}
	now := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	record, details, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
		RefreshToken: " 1//fake-refresh ",
		Client:       client,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ImportRefreshToken: %v", err)
	}
	if client.refreshed != "1//fake-refresh" {
		t.Fatalf("refreshed %q", client.refreshed)
	}
	if record.Provider != "antigravity" {
		t.Fatalf("provider = %q", record.Provider)
	}
	if record.Metadata["refresh_token"] != "1//fake-refresh" || record.Metadata["email"] != "katherine@example.dev" {
		t.Fatalf("metadata = %+v; Google omits the refresh token, the imported one must be kept", record.Metadata)
	}
	if record.Metadata["project_id"] != "project-fake-1" {
		t.Fatalf("project_id = %v", record.Metadata["project_id"])
	}
	if details.Email != "katherine@example.dev" {
		t.Fatalf("details = %+v", details)
	}
}

func TestImportRefreshTokenToleratesAMissingProject(t *testing.T) {
	record, _, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
		RefreshToken: "1//fake-refresh",
		Client:       &fakeRefreshClient{projectErr: errors.New("loadCodeAssist 500")},
	})
	if err != nil {
		t.Fatalf("a missing project must not fail the import: %v", err)
	}
	if record == nil {
		t.Fatal("record is nil")
	}
}

func TestImportRefreshTokenClassifiesRefusals(t *testing.T) {
	cases := []error{
		errors.New(`antigravity token refresh: request failed: status 400: {"error":"invalid_grant"}`),
		errors.New(`antigravity token refresh: request failed: status 401: {"error":"unauthorized_client"}`),
	}
	for _, upstream := range cases {
		_, _, err := ImportRefreshToken(context.Background(), RefreshTokenImportOptions{
			RefreshToken: "1//fake-refresh",
			Client:       &fakeRefreshClient{refreshErr: upstream},
		})
		if !errors.Is(err, credentialimport.ErrInvalidCredential) {
			t.Errorf("%v: error = %v, want ErrInvalidCredential", upstream, err)
		}
	}
}

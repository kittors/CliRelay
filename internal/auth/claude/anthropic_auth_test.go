package claude

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateAuthURLWithRedirectURIUsesProvidedRedirect(t *testing.T) {
	auth := &ClaudeAuth{}
	pkceCodes := &PKCECodes{
		CodeVerifier:  "verifier",
		CodeChallenge: "challenge",
	}

	authURL, state, err := auth.GenerateAuthURLWithRedirectURI("state-123", pkceCodes, PlatformRedirectURI)
	if err != nil {
		t.Fatalf("GenerateAuthURLWithRedirectURI returned error: %v", err)
	}
	if state != "state-123" {
		t.Fatalf("state = %q, want %q", state, "state-123")
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got := parsed.Query().Get("redirect_uri"); got != PlatformRedirectURI {
		t.Fatalf("redirect_uri = %q, want %q", got, PlatformRedirectURI)
	}
}

func TestExchangeCodeForTokensWithRedirectURIUsesProvidedRedirect(t *testing.T) {
	var requestBody map[string]any
	auth := &ClaudeAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != TokenURL {
					t.Fatalf("request URL = %q, want %q", req.URL.String(), TokenURL)
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err := json.Unmarshal(body, &requestBody); err != nil {
					t.Fatalf("unmarshal request body: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{
						"access_token":"access-token",
						"refresh_token":"refresh-token",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"uuid":"account-uuid-123","email_address":"user@example.com"}
					}`)),
					Request: req,
				}, nil
			}),
		},
	}

	bundle, err := auth.ExchangeCodeForTokensWithRedirectURI(context.Background(), "code-123", "state-123", &PKCECodes{
		CodeVerifier:  "verifier",
		CodeChallenge: "challenge",
	}, PlatformRedirectURI)
	if err != nil {
		t.Fatalf("ExchangeCodeForTokensWithRedirectURI returned error: %v", err)
	}
	if bundle.TokenData.Email != "user@example.com" {
		t.Fatalf("email = %q, want %q", bundle.TokenData.Email, "user@example.com")
	}
	if bundle.TokenData.AccountUUID != "account-uuid-123" {
		t.Fatalf("account uuid = %q, want %q", bundle.TokenData.AccountUUID, "account-uuid-123")
	}
	if got := requestBody["redirect_uri"]; got != PlatformRedirectURI {
		t.Fatalf("redirect_uri = %v, want %q", got, PlatformRedirectURI)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

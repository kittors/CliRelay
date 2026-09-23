package antigravity

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type discoveryRoundTripper func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFetchAccountDetailsUsesAntigravityClientIdentity(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != APIEndpoint+"/"+APIVersion+":loadCodeAssist" {
			return nil, fmt.Errorf("unexpected discovery request: %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("User-Agent"); got != ClientUserAgent {
			t.Errorf("discovery User-Agent = %q, want %q", got, ClientUserAgent)
			return nil, fmt.Errorf("discovery must identify the Antigravity client")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"cloudaicompanionProject":"test-project","currentTier":{"id":"free-tier","name":"Antigravity"}}`)),
		}, nil
	})}

	info, err := NewAntigravityAuth(nil, client).FetchAccountDetails(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("FetchAccountDetails: %v", err)
	}
	if info.ProjectID != "test-project" || info.TierID != "free-tier" || info.PlanType != "Antigravity" {
		t.Fatalf("unexpected account details: %+v", info)
	}
}

func TestBuildAuthURLUsesDefaultAntigravityClient(t *testing.T) {
	t.Setenv(config.EnvAntigravityOAuthClientID, "")
	t.Setenv(config.EnvAntigravityOAuthClientSecret, "")

	auth := NewAntigravityAuth(&config.Config{}, nil)

	authURL := auth.BuildAuthURL("state-value", "http://localhost:51121/oauth-callback")
	if authURL == "" {
		t.Fatal("authURL is empty, want URL with default Antigravity client")
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	if got := parsed.Query().Get("client_id"); got != config.AntigravityOAuthClientID {
		t.Fatalf("client_id = %q, want default Antigravity client", got)
	}
	if got := parsed.Query().Get("state"); got != "state-value" {
		t.Fatalf("state = %q, want state-value", got)
	}
}

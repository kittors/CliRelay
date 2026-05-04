package antigravity

import (
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

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

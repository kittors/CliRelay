package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"golang.org/x/oauth2"
)

func TestEnrichOAuthTokenMapStoresClientSecret(t *testing.T) {
	tokenMap := map[string]any{
		"access_token": "access-token",
	}
	conf := &oauth2.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Scopes:       Scopes,
	}

	enriched := EnrichOAuthTokenMap(tokenMap, conf)

	if enriched["client_id"] != "client-id" {
		t.Fatalf("client_id = %v, want client-id", enriched["client_id"])
	}
	if enriched["client_secret"] != "client-secret" {
		t.Fatalf("client_secret = %v, want client-secret", enriched["client_secret"])
	}
	if enriched["token_uri"] != "https://oauth2.googleapis.com/token" {
		t.Fatalf("token_uri = %v, want oauth2 token endpoint", enriched["token_uri"])
	}
}

func TestResolveOAuthClientCredentialsUsesStoredTokenClient(t *testing.T) {
	clientID, clientSecret := ResolveOAuthClientCredentials(&config.Config{
		OAuthClients: config.OAuthClients{
			Gemini: config.OAuthClient{
				ClientID:     "configured-client-id",
				ClientSecret: "configured-client-secret",
			},
		},
	}, map[string]any{
		"client_id":     "stored-client-id",
		"client_secret": "stored-client-secret",
	}, nil)

	if clientID != "stored-client-id" {
		t.Fatalf("clientID = %q, want stored-client-id", clientID)
	}
	if clientSecret != "stored-client-secret" {
		t.Fatalf("clientSecret = %q, want stored-client-secret", clientSecret)
	}
}

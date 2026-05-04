package config

import "testing"

func TestGeminiOAuthClientCredentialsDefaultsToGeminiCLIClient(t *testing.T) {
	t.Setenv(EnvGeminiOAuthClientID, "")
	t.Setenv(EnvGeminiOAuthClientSecret, "")

	cfg := &Config{}

	clientID, clientSecret := cfg.OAuthClientCredentials(OAuthClientGemini)

	if clientID != "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com" {
		t.Fatalf("clientID = %q, want official Gemini CLI OAuth client", clientID)
	}
	if clientSecret == "" {
		t.Fatalf("clientSecret is empty, want official Gemini CLI OAuth client secret")
	}
}

func TestGeminiOAuthClientCredentialsKeepsExplicitConfig(t *testing.T) {
	t.Setenv(EnvGeminiOAuthClientID, "")
	t.Setenv(EnvGeminiOAuthClientSecret, "")

	cfg := &Config{
		OAuthClients: OAuthClients{
			Gemini: OAuthClient{
				ClientID:     "custom-client-id",
				ClientSecret: "custom-client-secret",
			},
		},
	}

	clientID, clientSecret := cfg.OAuthClientCredentials(OAuthClientGemini)

	if clientID != "custom-client-id" {
		t.Fatalf("clientID = %q, want custom-client-id", clientID)
	}
	if clientSecret != "custom-client-secret" {
		t.Fatalf("clientSecret = %q, want custom-client-secret", clientSecret)
	}
}

func TestAntigravityOAuthClientCredentialsDefaultsToAntigravityClient(t *testing.T) {
	t.Setenv(EnvAntigravityOAuthClientID, "")
	t.Setenv(EnvAntigravityOAuthClientSecret, "")

	cfg := &Config{}

	clientID, clientSecret := cfg.OAuthClientCredentials(OAuthClientAntigravity)

	if clientID != AntigravityOAuthClientID {
		t.Fatalf("clientID = %q, want official Antigravity OAuth client", clientID)
	}
	if clientSecret != AntigravityOAuthClientSecret {
		t.Fatalf("clientSecret = %q, want official Antigravity OAuth client secret", clientSecret)
	}
}

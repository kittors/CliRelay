package config

import (
	"os"
	"strings"
)

const (
	OAuthClientGemini      = "gemini"
	OAuthClientAntigravity = "antigravity"

	// GeminiCLIOAuthClientID and GeminiCLIOAuthClientSecret are the OAuth
	// client credentials published by Google's Gemini CLI for Code Assist login.
	GeminiCLIOAuthClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	GeminiCLIOAuthClientSecret = "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"

	// AntigravityOAuthClientID and AntigravityOAuthClientSecret are the OAuth
	// client credentials used by Antigravity for Google Code Assist login.
	AntigravityOAuthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	AntigravityOAuthClientSecret = "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"

	EnvGeminiOAuthClientID     = "CLIRELAY_GEMINI_OAUTH_CLIENT_ID"
	EnvGeminiOAuthClientSecret = "CLIRELAY_GEMINI_OAUTH_CLIENT_SECRET"

	EnvAntigravityOAuthClientID     = "CLIRELAY_ANTIGRAVITY_OAUTH_CLIENT_ID"
	EnvAntigravityOAuthClientSecret = "CLIRELAY_ANTIGRAVITY_OAUTH_CLIENT_SECRET"
)

// OAuthClients groups optional OAuth2 client credentials.
type OAuthClients struct {
	Gemini      OAuthClient `yaml:"gemini"`
	Antigravity OAuthClient `yaml:"antigravity"`
}

// OAuthClient stores a single OAuth2 client credential pair.
type OAuthClient struct {
	ClientID     string `yaml:"client-id"`
	ClientSecret string `yaml:"client-secret"`
}

func (cfg *Config) OAuthClientCredentials(kind string) (clientID, clientSecret string) {
	if cfg != nil {
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case OAuthClientGemini:
			clientID = strings.TrimSpace(cfg.OAuthClients.Gemini.ClientID)
			clientSecret = strings.TrimSpace(cfg.OAuthClients.Gemini.ClientSecret)
		case OAuthClientAntigravity:
			clientID = strings.TrimSpace(cfg.OAuthClients.Antigravity.ClientID)
			clientSecret = strings.TrimSpace(cfg.OAuthClients.Antigravity.ClientSecret)
		}
	}

	// Env fallback (allows running without storing credentials in config.yaml).
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case OAuthClientGemini:
		if clientID == "" {
			clientID = strings.TrimSpace(os.Getenv(EnvGeminiOAuthClientID))
		}
		if clientSecret == "" {
			clientSecret = strings.TrimSpace(os.Getenv(EnvGeminiOAuthClientSecret))
		}
	case OAuthClientAntigravity:
		if clientID == "" {
			clientID = strings.TrimSpace(os.Getenv(EnvAntigravityOAuthClientID))
		}
		if clientSecret == "" {
			clientSecret = strings.TrimSpace(os.Getenv(EnvAntigravityOAuthClientSecret))
		}
	}

	if strings.EqualFold(strings.TrimSpace(kind), OAuthClientGemini) {
		if clientID == "" {
			clientID = GeminiCLIOAuthClientID
		}
		if clientSecret == "" && clientID == GeminiCLIOAuthClientID {
			clientSecret = GeminiCLIOAuthClientSecret
		}
	}
	if strings.EqualFold(strings.TrimSpace(kind), OAuthClientAntigravity) {
		if clientID == "" {
			clientID = AntigravityOAuthClientID
		}
		if clientSecret == "" && clientID == AntigravityOAuthClientID {
			clientSecret = AntigravityOAuthClientSecret
		}
	}

	return clientID, clientSecret
}

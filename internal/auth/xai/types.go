// Package xai provides OAuth2 authentication helpers for xAI Grok.
package xai

import "time"

const (
	// DefaultAPIBaseURL is the official xAI API base URL.
	DefaultAPIBaseURL = "https://api.x.ai/v1"
	// CLIChatProxyBaseURL is the Grok Build/CLI endpoint for non-media HTTP chat.
	CLIChatProxyBaseURL = "https://cli-chat-proxy.grok.com/v1"
	Issuer              = "https://auth.x.ai"
	DiscoveryURL        = Issuer + "/.well-known/openid-configuration"
	ClientID            = "b1a00492-073a-47ea-816f-4c329264a828"
	Scope               = "openid profile email offline_access grok-cli:access api:access"
	RedirectHost        = "127.0.0.1"
	CallbackPort        = 56121
	RedirectPath        = "/callback"
)

var refreshLead = 5 * time.Minute

func RefreshLead() time.Duration { return refreshLead }

type PKCECodes struct {
	CodeVerifier  string
	CodeChallenge string
}

type AuthorizeURLParams struct {
	AuthorizationEndpoint string
	RedirectURI           string
	CodeChallenge         string
	State                 string
	Nonce                 string
}

type Discovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Expire       string `json:"expired,omitempty"`
	Email        string `json:"email,omitempty"`
	Subject      string `json:"sub,omitempty"`
}

type AuthBundle struct {
	TokenData     TokenData
	LastRefresh   string
	BaseURL       string
	RedirectURI   string
	TokenEndpoint string
}

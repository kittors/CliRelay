package executor

import (
	"net/http"
	"net/http/httptest"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestXAIChatBaseURL(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "nil auth uses api", want: xaiauth.DefaultAPIBaseURL},
		{name: "non oauth uses api", auth: &cliproxyauth.Auth{Provider: "xai"}, want: xaiauth.DefaultAPIBaseURL},
		{
			name: "historical oauth defaults to cli",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind": "oauth",
				"base_url":  xaiauth.DefaultAPIBaseURL + "/",
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "metadata oauth defaults to cli",
			auth: &cliproxyauth.Auth{Metadata: map[string]any{
				"auth_kind": "oauth",
				"base_url":  xaiauth.DefaultAPIBaseURL,
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "using api true keeps official api",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"auth_kind": "oauth",
				"base_url":  xaiauth.DefaultAPIBaseURL,
				"using_api": "true",
			}},
			want: xaiauth.DefaultAPIBaseURL,
		},
		{
			name: "using api false selects cli",
			auth: &cliproxyauth.Auth{Metadata: map[string]any{
				"base_url":  xaiauth.DefaultAPIBaseURL,
				"using_api": false,
			}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "custom gateway remains custom",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url":  "https://gateway.example.com/v1",
				"using_api": "false",
			}},
			want: "https://gateway.example.com/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := xaiChatBaseURL(tt.auth); got != tt.want {
				t.Fatalf("xaiChatBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyXAIChatHeaders(t *testing.T) {
	t.Run("cli endpoint receives cli identity headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  xaiauth.DefaultAPIBaseURL,
		}}

		applyXAIChatHeaders(req, nil, auth, "oauth-token", true)

		if got := req.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q, want Bearer oauth-token", got)
		}
		if got := req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
			t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
		}
		if got := req.Header.Get("X-Grok-Client-Version"); got != config.DefaultXAIFingerprintClientVersion {
			t.Fatalf("X-Grok-Client-Version = %q, want %q", got, config.DefaultXAIFingerprintClientVersion)
		}
	})

	t.Run("api endpoint omits cli identity headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, xaiauth.DefaultAPIBaseURL+"/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{
			"auth_kind": "oauth",
			"base_url":  xaiauth.DefaultAPIBaseURL,
			"using_api": "true",
		}}

		applyXAIChatHeaders(req, nil, auth, "oauth-token", false)

		if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
			t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
		}
		if got := req.Header.Get("X-Grok-Client-Version"); got != "" {
			t.Fatalf("X-Grok-Client-Version = %q, want empty", got)
		}
	})

	t.Run("custom gateway omits cli identity headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "https://gateway.example.com/v1/responses", nil)
		auth := &cliproxyauth.Auth{Attributes: map[string]string{
			"base_url":  "https://gateway.example.com/v1",
			"using_api": "false",
		}}

		applyXAIChatHeaders(req, nil, auth, "oauth-token", false)

		if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
			t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
		}
	})
}

func TestXAIPrepareRequestKeepsGenericRequestsOnAPIHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, xaiauth.DefaultAPIBaseURL+"/images/generations", nil)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"auth_kind": "oauth",
		"base_url":  xaiauth.DefaultAPIBaseURL,
	}}

	if err := NewXAIExecutor(nil).PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
		t.Fatalf("%s = %q, want empty for generic API request", xaiTokenAuthHeader, got)
	}
}

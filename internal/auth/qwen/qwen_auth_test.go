package qwen

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewQwenAuthUsesConfiguredOAuthUserAgent(t *testing.T) {
	cfg := &config.Config{OAuthUserAgent: "CustomAgent/1.0"}

	auth := NewQwenAuth(cfg)
	if auth.userAgent != "CustomAgent/1.0" {
		t.Fatalf("userAgent = %q, want %q", auth.userAgent, "CustomAgent/1.0")
	}
}

func TestNewQwenAuthFallsBackToDefaultOAuthUserAgent(t *testing.T) {
	auth := NewQwenAuth(&config.Config{})
	if auth.userAgent != defaultOAuthUserAgent {
		t.Fatalf("userAgent = %q, want %q", auth.userAgent, defaultOAuthUserAgent)
	}
}

func TestRefreshTokensSetsConfiguredUserAgent(t *testing.T) {
	var gotUserAgent string
	auth := &QwenAuth{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotUserAgent = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"token","refresh_token":"refresh","token_type":"Bearer","expires_in":60}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
		userAgent: "CustomAgent/1.0",
	}

	_, err := auth.RefreshTokens(context.Background(), "refresh-token")
	if err != nil {
		t.Fatalf("RefreshTokens() error = %v", err)
	}
	if gotUserAgent != "CustomAgent/1.0" {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, "CustomAgent/1.0")
	}
}

func TestInitiateDeviceFlowSetsConfiguredUserAgent(t *testing.T) {
	var gotUserAgent string
	auth := &QwenAuth{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotUserAgent = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"device_code":"device","user_code":"user","verification_uri":"https://example.com","verification_uri_complete":"https://example.com/complete","expires_in":600,"interval":5}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
		userAgent: "CustomAgent/1.0",
	}

	_, err := auth.InitiateDeviceFlow(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceFlow() error = %v", err)
	}
	if gotUserAgent != "CustomAgent/1.0" {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, "CustomAgent/1.0")
	}
}

func TestPollForTokenUsesConfiguredHTTPClientAndUserAgent(t *testing.T) {
	originalDefaultTransport := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = originalDefaultTransport
	})

	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(`{"error":"wrong_client","error_description":"default transport used"}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	var gotUserAgent string
	called := false
	auth := &QwenAuth{
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			gotUserAgent = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"token","refresh_token":"refresh","token_type":"Bearer","expires_in":60}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
		userAgent: "CustomAgent/1.0",
	}

	_, err := auth.PollForToken("device-code", "code-verifier")
	if err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if !called {
		t.Fatalf("expected PollForToken to use QwenAuth httpClient")
	}
	if gotUserAgent != "CustomAgent/1.0" {
		t.Fatalf("User-Agent = %q, want %q", gotUserAgent, "CustomAgent/1.0")
	}
}

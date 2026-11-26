package codex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	var calls int32
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","code":"refresh_token_reused"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected error for non-retryable refresh failure")
	}
	if !cliproxyauth.IsPermanentAuthError(err) {
		t.Fatalf("expected PermanentAuthError, got: %T %v", err, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused") {
		t.Fatalf("expected refresh_token_reused in error, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", got)
	}
}

func TestRefreshTokensWithRetry_InvalidGrant_IsPermanent(t *testing.T) {
	var calls int32
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"Refresh token is invalid"}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "expired_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected error for invalid_grant")
	}
	if !cliproxyauth.IsPermanentAuthError(err) {
		t.Fatalf("expected PermanentAuthError for invalid_grant, got: %T %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt for invalid_grant, got %d", got)
	}
}

func TestRefreshTokensWithRetry_AllPermanentCodesOnlyAttemptOnce(t *testing.T) {
	cases := []struct {
		name string
		code string
		body string
	}{
		{
			name: "invalid_refresh_token",
			code: "invalid_refresh_token",
			body: `{"error":{"message":"Provided refresh token is invalid.","type":"invalid_request_error","code":"invalid_refresh_token"}}`,
		},
		{
			name: "refresh_token_invalidated",
			code: "refresh_token_invalidated",
			body: `{"error":{"message":"Your refresh token has been invalidated.","type":"invalid_request_error","code":"refresh_token_invalidated"}}`,
		},
		{
			name: "refresh_token_expired",
			code: "refresh_token_expired",
			body: `{"error":{"message":"Refresh token expired.","type":"invalid_request_error","code":"refresh_token_expired"}}`,
		},
		{
			name: "token_revoked",
			code: "token_revoked",
			body: `{"error":{"message":"Encountered invalidated oauth token for user","code":"token_revoked"}}`,
		},
		{
			name: "account_deactivated",
			code: "account_deactivated",
			body: `{"error":{"message":"Account deactivated","code":"account_deactivated"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			auth := &CodexAuth{
				httpClient: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						atomic.AddInt32(&calls, 1)
						return &http.Response{
							StatusCode: http.StatusUnauthorized,
							Body:       io.NopCloser(strings.NewReader(tc.body)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}),
				},
			}

			_, err := auth.RefreshTokensWithRetry(context.Background(), "dead_refresh_token", 3)
			if err == nil {
				t.Fatalf("expected error for %s", tc.code)
			}
			if !cliproxyauth.IsPermanentAuthError(err) {
				t.Fatalf("expected PermanentAuthError for %s, got %T %v", tc.code, err, err)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("expected 1 refresh attempt for %s, got %d", tc.code, got)
			}
		})
	}
}

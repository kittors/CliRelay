package codex

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
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

func TestCodexAuthRequestsCarryCanonicalIdentityHeadersWithoutVersion(t *testing.T) {
	var seenUA, seenOriginator, seenVersion string
	captureTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seenUA = req.Header.Get("User-Agent")
		seenOriginator = req.Header.Get("Originator")
		seenVersion = req.Header.Get("Version")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"at","refresh_token":"rt2","id_token":"","expires_in":3600}`)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	t.Run("zero-value CodexAuth falls back to official defaults", func(t *testing.T) {
		seenUA, seenOriginator, seenVersion = "", "", ""
		svc := &CodexAuth{httpClient: &http.Client{Transport: captureTransport}}
		if _, err := svc.RefreshTokens(context.Background(), "rt-zero"); err != nil {
			t.Fatalf("RefreshTokens: %v", err)
		}
		if seenUA != config.DefaultCodexFingerprintUserAgent {
			t.Fatalf("User-Agent = %q, want %q", seenUA, config.DefaultCodexFingerprintUserAgent)
		}
		if seenOriginator != config.DefaultCodexFingerprintOriginator {
			t.Fatalf("Originator = %q, want %q", seenOriginator, config.DefaultCodexFingerprintOriginator)
		}
		if seenVersion != "" {
			t.Fatalf("auth endpoint must not send Version header, got %q", seenVersion)
		}
	})

	t.Run("configured identity and WithClientIdentity apply to exchange and refresh", func(t *testing.T) {
		cfg := &config.Config{
			IdentityFingerprint: config.IdentityFingerprintConfig{
				Codex: config.CodexIdentityFingerprintConfig{
					Enabled:    true,
					UserAgent:  "codex_cli_rs/0.155.0 (Mac OS 26.5; arm64)",
					Originator: "codex_cli_rs",
				},
			},
		}
		svc := NewCodexAuthWithProxy(cfg, "")
		svc.httpClient = &http.Client{Transport: captureTransport}

		seenUA, seenOriginator, seenVersion = "", "", ""
		if _, err := svc.ExchangeCodeForTokensWithRedirect(context.Background(), "code-1", RedirectURI, &PKCECodes{CodeVerifier: "v"}); err != nil {
			t.Fatalf("ExchangeCodeForTokensWithRedirect: %v", err)
		}
		if seenUA != "codex_cli_rs/0.155.0 (Mac OS 26.5; arm64)" || seenOriginator != "codex_cli_rs" || seenVersion != "" {
			t.Fatalf("exchange headers = (%q, %q, %q)", seenUA, seenOriginator, seenVersion)
		}

		svc.WithClientIdentity("Codex Desktop/0.160.0 (Mac OS 26.5; arm64)", "Codex Desktop")
		seenUA, seenOriginator, seenVersion = "", "", ""
		if _, err := svc.RefreshTokensWithRetry(context.Background(), "rt-custom", 2); err != nil {
			t.Fatalf("RefreshTokensWithRetry: %v", err)
		}
		if seenUA != "Codex Desktop/0.160.0 (Mac OS 26.5; arm64)" || seenOriginator != "Codex Desktop" || seenVersion != "" {
			t.Fatalf("refresh headers = (%q, %q, %q)", seenUA, seenOriginator, seenVersion)
		}
	})
}

func TestRefreshTokensWithRetry_HTTP401AlwaysPermanentWithoutRetry(t *testing.T) {
	var calls int32
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"brand_new_401_reason"}}`)),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "rt-unknown-401", 3)
	if err == nil || !cliproxyauth.IsPermanentAuthError(err) {
		t.Fatalf("expected PermanentAuthError for HTTP 401, got: %T %v", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 attempt for HTTP 401, got %d", got)
	}
}

func TestRefreshTokensWithRetry_NonRetryable4xxAnd500DoNotReplayOrMarkPermanent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "cloudflare 403", status: http.StatusForbidden, body: `<html>Just a moment...</html>`},
		{name: "unsupported region 403", status: http.StatusForbidden, body: `{"error":{"code":"unsupported_country_region_territory"}}`},
		{name: "internal server error 500", status: http.StatusInternalServerError, body: `{"error":"internal_error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			auth := &CodexAuth{
				httpClient: &http.Client{
					Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						atomic.AddInt32(&calls, 1)
						return &http.Response{
							StatusCode: tc.status,
							Body:       io.NopCloser(strings.NewReader(tc.body)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}),
				},
			}

			_, err := auth.RefreshTokensWithRetry(context.Background(), "rt-"+tc.name, 3)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if cliproxyauth.IsPermanentAuthError(err) {
				t.Fatalf("%s must not be marked PermanentAuthError: %v", tc.name, err)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("expected 1 attempt for %s (no replay), got %d", tc.name, got)
			}
		})
	}
}

func TestRefreshTokensWithRetry_DoesNotReplayAfterRequestWrittenToWire(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Drop the TCP connection after receiving the POST so the client sees
		// EOF after WroteRequest: upstream may already have rotated the token.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not support Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	baseTransport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}
	defer baseTransport.CloseIdleConnections()

	// Rewrite https://auth.openai.com/oauth/token to the plain HTTP test server
	// while keeping net/http's real transport + httptrace.WroteRequest intact.
	auth := &CodexAuth{
		httpClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				cloned := req.Clone(req.Context())
				cloned.URL.Scheme = "http"
				cloned.URL.Host = srv.Listener.Addr().String()
				return baseTransport.RoundTrip(cloned)
			}),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "rt-post-write-eof", 3)
	if err == nil {
		t.Fatal("expected error on dropped connection")
	}
	if cliproxyauth.IsPermanentAuthError(err) {
		t.Fatalf("dropped connection must not be marked PermanentAuthError: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server received %d requests after post-write drop, want 1 (must not replay single-use refresh_token)", got)
	}
}

func TestRefreshTokensWithRetry_RetriesPreSendDialFailureAndTransient503(t *testing.T) {
	t.Run("pre-send transport error retries and recovers", func(t *testing.T) {
		var calls int32
		auth := &CodexAuth{
			httpClient: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					n := atomic.AddInt32(&calls, 1)
					if n == 1 {
						return nil, errors.New("dial tcp: connection refused")
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-ok","refresh_token":"rt-ok","expires_in":3600}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}),
			},
		}
		td, err := auth.RefreshTokensWithRetry(context.Background(), "rt-dial-retry", 3)
		if err != nil {
			t.Fatalf("expected recovery after pre-send error, got: %v", err)
		}
		if td == nil || td.RefreshToken != "rt-ok" {
			t.Fatalf("tokenData = %+v, want rt-ok", td)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})

	t.Run("transient 503 retries and recovers", func(t *testing.T) {
		var calls int32
		auth := &CodexAuth{
			httpClient: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					n := atomic.AddInt32(&calls, 1)
					if n == 1 {
						return &http.Response{
							StatusCode: http.StatusServiceUnavailable,
							Body:       io.NopCloser(strings.NewReader(`service unavailable`)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"access_token":"at-503","refresh_token":"rt-503","expires_in":3600}`)),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}),
			},
		}
		td, err := auth.RefreshTokensWithRetry(context.Background(), "rt-503-retry", 3)
		if err != nil {
			t.Fatalf("expected recovery after 503, got: %v", err)
		}
		if td == nil || td.RefreshToken != "rt-503" {
			t.Fatalf("tokenData = %+v, want rt-503", td)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})
}

// Package codex provides authentication and token management for OpenAI's Codex API.
// It handles the OAuth2 flow, including generating authorization URLs, exchanging
// authorization codes for tokens, and refreshing expired tokens. The package also
// defines data structures for storing and managing Codex authentication credentials.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/refreshguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// OAuth configuration constants for OpenAI Codex
const (
	AuthURL     = "https://auth.openai.com/oauth/authorize"
	TokenURL    = "https://auth.openai.com/oauth/token"
	ClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	RedirectURI = "http://localhost:1455/auth/callback"
)

// CodexAuth handles the OpenAI OAuth2 authentication flow.
// It manages the HTTP client and provides methods for generating authorization URLs,
// exchanging authorization codes for tokens, and refreshing access tokens.
type CodexAuth struct {
	httpClient *http.Client
	userAgent  string
	originator string
}

// NewCodexAuth creates a new CodexAuth service instance.
// It initializes an HTTP client with proxy settings from the provided configuration.
func NewCodexAuth(cfg *config.Config) *CodexAuth {
	return NewCodexAuthWithProxy(cfg, "")
}

// NewCodexAuthWithProxy is NewCodexAuth pinned to one egress proxy.
//
// Token refresh has to leave from the same address as the credential's own
// requests; see resolveAuthProxyURL in the executor package for what goes wrong
// when the two diverge. An empty proxyURL keeps the configured default.
func NewCodexAuthWithProxy(cfg *config.Config, proxyURL string) *CodexAuth {
	sdkCfg := sdkConfigWithProxy(cfg, proxyURL)
	userAgent, originator := resolveCodexAuthClientIdentity(cfg)
	return &CodexAuth{
		httpClient: util.SetProxy(sdkCfg, util.NewHTTPClient(util.DefaultHTTPClientTimeout)),
		userAgent:  userAgent,
		originator: originator,
	}
}

// WithClientIdentity pins the User-Agent and Originator headers presented to
// auth.openai.com so token refreshes share the same official Codex client
// identity as the credential's inference traffic.
func (o *CodexAuth) WithClientIdentity(userAgent, originator string) *CodexAuth {
	if o == nil {
		return nil
	}
	if trimmed := strings.TrimSpace(userAgent); trimmed != "" {
		o.userAgent = trimmed
	}
	if trimmed := strings.TrimSpace(originator); trimmed != "" {
		o.originator = trimmed
	}
	return o
}

// ApplyAuthIdentityHeaders writes the canonical Codex auth-plane headers
// (User-Agent and Originator, deliberately omitting inference-only Version)
// onto a request bound for auth.openai.com.
func ApplyAuthIdentityHeaders(req *http.Request, cfg *config.Config) {
	if req == nil {
		return
	}
	userAgent, originator := resolveCodexAuthClientIdentity(cfg)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", originator)
}

func resolveCodexAuthClientIdentity(cfg *config.Config) (userAgent, originator string) {
	if cfg != nil && cfg.IdentityFingerprint.Codex.Enabled {
		fp := config.NormalizeCodexIdentityFingerprint(cfg.IdentityFingerprint.Codex)
		userAgent = strings.TrimSpace(fp.UserAgent)
		originator = strings.TrimSpace(fp.Originator)
	}
	if userAgent == "" {
		userAgent = config.DefaultCodexFingerprintUserAgent
	}
	if originator == "" {
		originator = config.DefaultCodexFingerprintOriginator
	}
	return userAgent, originator
}

// applyIdentityHeaders keeps auth.openai.com calls aligned with official
// codex-rs auth traffic (default_headers sends User-Agent + originator without
// inference-only Version) so refreshes never go out as bare Go-http-client.
func (o *CodexAuth) applyIdentityHeaders(req *http.Request) {
	if req == nil {
		return
	}
	userAgent := strings.TrimSpace(o.userAgent)
	if userAgent == "" {
		userAgent = config.DefaultCodexFingerprintUserAgent
	}
	originator := strings.TrimSpace(o.originator)
	if originator == "" {
		originator = config.DefaultCodexFingerprintOriginator
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", originator)
}

// sdkConfigWithProxy copies the SDK config with proxyURL substituted, so pinning
// an egress proxy cannot mutate shared configuration.
func sdkConfigWithProxy(cfg *config.Config, proxyURL string) *config.SDKConfig {
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
	}
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		sdkCfg.ProxyURL = proxyURL
	}
	return &sdkCfg
}

// GenerateAuthURL creates the OAuth authorization URL with PKCE (Proof Key for Code Exchange).
// It constructs the URL with the necessary parameters, including the client ID,
// response type, redirect URI, scopes, and PKCE challenge.
func (o *CodexAuth) GenerateAuthURL(state string, pkceCodes *PKCECodes) (string, error) {
	if pkceCodes == nil {
		return "", fmt.Errorf("PKCE codes are required")
	}

	params := url.Values{
		"client_id":                  {ClientID},
		"response_type":              {"code"},
		"redirect_uri":               {RedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {pkceCodes.CodeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}

	authURL := fmt.Sprintf("%s?%s", AuthURL, params.Encode())
	return authURL, nil
}

// ExchangeCodeForTokens exchanges an authorization code for access and refresh tokens.
// It performs an HTTP POST request to the OpenAI token endpoint with the provided
// authorization code and PKCE verifier.
func (o *CodexAuth) ExchangeCodeForTokens(ctx context.Context, code string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	return o.ExchangeCodeForTokensWithRedirect(ctx, code, RedirectURI, pkceCodes)
}

// ExchangeCodeForTokensWithRedirect exchanges an authorization code for tokens using
// a caller-provided redirect URI. This supports alternate auth flows such as device
// login while preserving the existing token parsing and storage behavior.
func (o *CodexAuth) ExchangeCodeForTokensWithRedirect(ctx context.Context, code, redirectURI string, pkceCodes *PKCECodes) (*CodexAuthBundle, error) {
	if pkceCodes == nil {
		return nil, fmt.Errorf("PKCE codes are required for token exchange")
	}
	if strings.TrimSpace(redirectURI) == "" {
		return nil, fmt.Errorf("redirect URI is required for token exchange")
	}

	// Prepare token exchange request
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
		"code_verifier": {pkceCodes.CodeVerifier},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	o.applyIdentityHeaders(req)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := util.ReadHTTPResponseBody("codex-oauth", resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}
	// log.Debugf("Token response: %s", string(body))

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Parse token response
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.GetUserEmail()
	}

	// Create token data
	tokenData := CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}

	// Create auth bundle
	bundle := &CodexAuthBundle{
		TokenData:   tokenData,
		LastRefresh: time.Now().Format(time.RFC3339),
	}

	return bundle, nil
}

// RefreshTokens refreshes an access token using a refresh token.
// This method is called when an access token has expired. It makes a request to the
// token endpoint to obtain a new set of tokens.
//
// OpenAI refresh tokens are single-use, so the exchange runs through
// refreshguard: it finishes even if ctx ends, and concurrent refreshes of one
// token share a single request.
func (o *CodexAuth) RefreshTokens(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	return refreshguard.Exchange(ctx, refreshGuardProvider, refreshToken, func(ctx context.Context) (*CodexTokenData, error) {
		return o.refreshOnce(ctx, refreshToken)
	})
}

// refreshGuardProvider scopes Codex exchanges in refreshguard; RefreshTokens and
// RefreshTokensWithRetry share it, so either one joins the other's exchange.
const refreshGuardProvider = "codex"

// refreshAttemptError distinguishes failures before the request reached
// auth.openai.com from ambiguous in-flight errors and HTTP status rejections,
// so single-use refresh tokens are never blindly replayed after transmission.
type refreshAttemptError struct {
	statusCode  int
	permanent   bool
	retryable   bool
	requestSent bool
	cause       error
}

func (e *refreshAttemptError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *refreshAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (o *CodexAuth) refreshOnce(ctx context.Context, refreshToken string) (*CodexTokenData, error) {
	data := url.Values{
		"client_id":     {ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}

	var requestSent atomic.Bool
	traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				requestSent.Store(true)
			}
		},
	})

	req, err := http.NewRequestWithContext(traceCtx, "POST", TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, &refreshAttemptError{
			retryable: false,
			cause:     fmt.Errorf("failed to create refresh request: %w", err),
		}
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	o.applyIdentityHeaders(req)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		sent := requestSent.Load()
		return nil, &refreshAttemptError{
			retryable:   !sent && ctx.Err() == nil,
			requestSent: sent,
			cause:       fmt.Errorf("token refresh request failed: %w", err),
		}
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := util.ReadHTTPResponseBody("codex-oauth", resp.Body)
	if err != nil {
		// Response headers were already received, so the upstream may have
		// rotated the single-use refresh token. Do not replay automatically.
		return nil, &refreshAttemptError{
			statusCode:  resp.StatusCode,
			retryable:   false,
			requestSent: true,
			cause:       fmt.Errorf("failed to read refresh response: %w", err),
		}
	}

	if resp.StatusCode != http.StatusOK {
		statusErr := fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
		permanent := isPermanentRefreshStatus(resp.StatusCode, statusErr)
		retryable := !permanent && isRetryableRefreshStatus(resp.StatusCode)
		return nil, &refreshAttemptError{
			statusCode:  resp.StatusCode,
			permanent:   permanent,
			retryable:   retryable,
			requestSent: true,
			cause:       statusErr,
		}
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, &refreshAttemptError{
			statusCode:  resp.StatusCode,
			retryable:   false,
			requestSent: true,
			cause:       fmt.Errorf("failed to parse refresh response: %w", err),
		}
	}

	// Extract account ID from ID token
	claims, err := ParseJWTToken(tokenResp.IDToken)
	if err != nil {
		log.Warnf("Failed to parse refreshed ID token: %v", err)
	}

	accountID := ""
	email := ""
	if claims != nil {
		accountID = claims.GetAccountID()
		email = claims.Email
	}

	return &CodexTokenData{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		AccountID:    accountID,
		Email:        email,
		Expire:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

// CreateTokenStorage creates a new CodexTokenStorage from a CodexAuthBundle.
// It populates the storage struct with token data, user information, and timestamps.
func (o *CodexAuth) CreateTokenStorage(bundle *CodexAuthBundle) *CodexTokenStorage {
	storage := &CodexTokenStorage{
		IDToken:      bundle.TokenData.IDToken,
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		AccountID:    bundle.TokenData.AccountID,
		LastRefresh:  bundle.LastRefresh,
		Email:        bundle.TokenData.Email,
		Expire:       bundle.TokenData.Expire,
	}

	return storage
}

// RefreshTokensWithRetry refreshes tokens with a built-in retry mechanism.
// It attempts to refresh the tokens up to a specified maximum number of retries,
// with an exponential backoff strategy to handle transient network errors.
//
// The attempts run as one refreshguard exchange: they continue if ctx ends, and
// a concurrent refresh of the same token waits for their result instead of
// presenting the token a second time.
func (o *CodexAuth) RefreshTokensWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("refresh token is required")
	}
	return refreshguard.Exchange(ctx, refreshGuardProvider, refreshToken, func(ctx context.Context) (*CodexTokenData, error) {
		return o.refreshWithRetry(ctx, refreshToken, maxRetries)
	})
}

func (o *CodexAuth) refreshWithRetry(ctx context.Context, refreshToken string, maxRetries int) (*CodexTokenData, error) {
	if maxRetries < 1 {
		maxRetries = 1
	}
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		tokenData, err := o.refreshOnce(ctx, refreshToken)
		if err == nil {
			return tokenData, nil
		}
		var attemptErr *refreshAttemptError
		if errors.As(err, &attemptErr) {
			if attemptErr.permanent {
				log.Warnf("Token refresh attempt %d failed with permanent error: %v", attempt+1, err)
				return nil, &cliproxyauth.PermanentAuthError{
					Reason: "credential revoked or expired",
					Cause:  err,
				}
			}
			if !attemptErr.retryable {
				// Either a non-retryable 4xx (such as Cloudflare/region 403) or
				// an ambiguous error after the request was already written to
				// the wire; do not replay the single-use refresh token.
				log.Warnf("Token refresh attempt %d failed without safe retry (request_sent=%v status=%d): %v",
					attempt+1, attemptErr.requestSent, attemptErr.statusCode, err)
				return nil, err
			}
		} else if isNonRetryableRefreshErr(err) {
			log.Warnf("Token refresh attempt %d failed with non-retryable error: %v", attempt+1, err)
			return nil, &cliproxyauth.PermanentAuthError{
				Reason: "credential revoked or expired",
				Cause:  err,
			}
		}

		lastErr = err
		log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
	}

	return nil, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}

func isPermanentRefreshStatus(statusCode int, err error) bool {
	// Matching official codex-rs auth manager behavior, any HTTP 401 from
	// auth.openai.com/oauth/token rejects the session permanently.
	if statusCode == http.StatusUnauthorized {
		return true
	}
	return isNonRetryableRefreshErr(err)
}

func isRetryableRefreshStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		// 4xx rejections (e.g. Cloudflare/geo 403) and 500s (which may occur
		// after token rotation) are surfaced once instead of replaying a
		// single-use refresh token in a tight loop.
		return false
	}
}

func isNonRetryableRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	raw := strings.ToLower(err.Error())
	// OpenAI's token endpoint returns these codes when the refresh token is
	// permanently dead (invalidated, expired, reused, or account revoked).
	// Retrying them wastes attempts and loops every 15m in the refresh worker.
	return strings.Contains(raw, "refresh_token_reused") ||
		strings.Contains(raw, "invalid_refresh_token") ||
		strings.Contains(raw, "refresh_token_invalidated") ||
		strings.Contains(raw, "refresh_token_expired") ||
		strings.Contains(raw, "invalid_grant") ||
		strings.Contains(raw, "token_revoked") ||
		strings.Contains(raw, "account_deactivated") ||
		strings.Contains(raw, "app_session_terminated") ||
		strings.Contains(raw, "invalid_client") ||
		strings.Contains(raw, "unauthorized_client") ||
		strings.Contains(raw, "access_denied") ||
		strings.Contains(raw, "invalid_scope") ||
		strings.Contains(raw, "token has been revoked") ||
		strings.Contains(raw, "token has been expired")
}

// UpdateTokenStorage updates an existing CodexTokenStorage with new token data.
// This is typically called after a successful token refresh to persist the new credentials.
func (o *CodexAuth) UpdateTokenStorage(storage *CodexTokenStorage, tokenData *CodexTokenData) {
	storage.IDToken = tokenData.IDToken
	storage.AccessToken = tokenData.AccessToken
	storage.RefreshToken = tokenData.RefreshToken
	storage.AccountID = tokenData.AccountID
	storage.LastRefresh = time.Now().Format(time.RFC3339)
	storage.Email = tokenData.Email
	storage.Expire = tokenData.Expire
}

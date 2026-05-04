package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/geminicli"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const defaultAPICallTimeout = 60 * time.Second
const (
	managementAPICallResponseLimit    int64 = 4 << 20
	managementOAuthTokenResponseLimit int64 = 64 << 10
)

var geminiOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

var antigravityOAuthTokenURL = "https://oauth2.googleapis.com/token"

type claudeOAuthRefresher interface {
	RefreshTokens(ctx context.Context, refreshToken string) (*claudeauth.ClaudeTokenData, error)
}

var newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return claudeauth.NewClaudeAuth(cfg)
}

const kimiOAuthClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
const kimiOAuthTokenURL = "https://auth.kimi.com/api/oauth/token"

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

// APICall makes a generic HTTP request on behalf of the management API caller.
// It is protected by the management middleware.
//
// Endpoint:
//
//	POST /v0/management/api-call
//
// Authentication:
//
//	Same as other management APIs (requires a management key and remote-management rules).
//	You can provide the key via:
//	- Authorization: Bearer <key>
//	- X-Management-Key: <key>
//
// Request JSON:
//   - auth_index / authIndex / AuthIndex (optional):
//     The credential "auth_index" from GET /v0/management/auth-files (or other endpoints returning it).
//     If omitted or not found, credential-specific proxy/token substitution is skipped.
//   - method (required): HTTP method, e.g. GET, POST, PUT, PATCH, DELETE.
//   - url (required): Absolute URL including scheme and host, e.g. "https://api.example.com/v1/ping".
//   - header (optional): Request headers map.
//     Supports magic variable "$TOKEN$" which is replaced using the selected credential:
//     1) metadata.access_token
//     2) attributes.api_key
//     3) metadata.token / metadata.id_token / metadata.cookie
//     Example: {"Authorization":"Bearer $TOKEN$"}.
//     Note: if you need to override the HTTP Host header, set header["Host"].
//   - data (optional): Raw request body as string (useful for POST/PUT/PATCH).
//
// Proxy selection (highest priority first):
//  1. Selected credential proxy_url
//  2. Global config proxy-url
//  3. Direct connect (environment proxies are not used)
//
// Response JSON (returned with HTTP 200 when the APICall itself succeeds):
//   - status_code: Upstream HTTP status code.
//   - header: Upstream response headers.
//   - body: Upstream response body as string.
//
// Example:
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"GET","url":"https://api.example.com/v1/ping","header":{"Authorization":"Bearer $TOKEN$"}}'
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer 831227" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"POST","url":"https://api.example.com/v1/fetchAvailableModels","header":{"Authorization":"Bearer $TOKEN$","Content-Type":"application/json","User-Agent":"cliproxyapi"},"data":"{}"}'
func (h *Handler) APICall(c *gin.Context) {
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndex(authIndex)

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}

	var hostOverride string
	var token string
	var tokenResolved bool
	var tokenErr error
	for key, value := range reqHeaders {
		if !strings.Contains(value, "$TOKEN$") {
			continue
		}
		if !tokenResolved {
			token, tokenErr = h.resolveTokenForAuth(c.Request.Context(), auth)
			tokenResolved = true
		}
		if auth != nil && token == "" {
			if tokenErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth token refresh failed"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token not found"})
			return
		}
		if token == "" {
			continue
		}
		reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	for key, value := range reqHeaders {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, value)
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	httpClient := util.NewHTTPClient(defaultAPICallTimeout)
	httpClient.Transport = h.apiCallTransport(auth)

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := bodyutil.ReadAll(resp.Body, managementAPICallResponseLimit)
	if errReadAll != nil {
		if bodyutil.IsTooLarge(errReadAll) {
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream response too large"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response"})
		return
	}

	c.JSON(http.StatusOK, apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(respBody),
	})
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if out := strings.TrimSpace(*v); out != "" {
			return out
		}
	}
	return ""
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := tokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		if v := tokenValueFromMetadata(shared.MetadataSnapshot()); v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) resolveTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "gemini-cli" {
		token, errToken := h.refreshGeminiOAuthAccessToken(ctx, auth)
		return token, errToken
	}
	if provider == "antigravity" {
		token, errToken := h.refreshAntigravityOAuthAccessToken(ctx, auth)
		return token, errToken
	}
	if provider == "claude" || provider == "anthropic" {
		token, errToken := h.refreshClaudeOAuthAccessToken(ctx, auth)
		return token, errToken
	}
	if provider == "kimi" {
		token, errToken := h.refreshKimiOAuthAccessToken(ctx, auth)
		return token, errToken
	}

	return tokenValueForAuth(auth), nil
}

func (h *Handler) refreshClaudeOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("claude oauth metadata missing")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(metadata))
	if current != "" && !claudeTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("claude refresh token missing")
	}

	refresher := newClaudeOAuthRefresher(h.claudeOAuthRefreshConfig(auth))
	tokenData, errRefresh := refresher.RefreshTokens(ctx, refreshToken)
	if errRefresh != nil {
		return "", errRefresh
	}
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return "", fmt.Errorf("claude oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = strings.TrimSpace(tokenData.AccessToken)
	if strings.TrimSpace(tokenData.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenData.RefreshToken)
	}
	if strings.TrimSpace(tokenData.Email) != "" {
		auth.Metadata["email"] = strings.TrimSpace(tokenData.Email)
	}
	if strings.TrimSpace(tokenData.Expire) != "" {
		auth.Metadata["expired"] = strings.TrimSpace(tokenData.Expire)
	}
	auth.Metadata["type"] = "claude"
	now := time.Now()
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)

	if h != nil && h.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		_, _ = h.authManager.Update(ctx, auth)
	}

	return strings.TrimSpace(tokenData.AccessToken), nil
}

func (h *Handler) claudeOAuthRefreshConfig(auth *coreauth.Auth) *config.Config {
	var cfgCopy config.Config
	if h != nil && h.cfg != nil {
		cfgCopy = *h.cfg
	}
	if auth == nil {
		return &cfgCopy
	}

	var proxyURL string
	if h != nil && h.cfg != nil {
		proxyURL = h.cfg.ResolveProxyURL(auth.ProxyID, auth.ProxyURL)
	} else {
		proxyURL = auth.ProxyURL
	}
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		cfgCopy.ProxyURL = trimmed
	}
	return &cfgCopy
}

func (h *Handler) refreshGeminiOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata, updater := geminiOAuthMetadata(auth)
	if len(metadata) == 0 {
		return "", fmt.Errorf("gemini oauth metadata missing")
	}

	base := make(map[string]any)
	if tokenRaw, ok := metadata["token"].(map[string]any); ok && tokenRaw != nil {
		base = cloneMap(tokenRaw)
	}

	var token oauth2.Token
	if len(base) > 0 {
		if raw, errMarshal := json.Marshal(base); errMarshal == nil {
			_ = json.Unmarshal(raw, &token)
		}
	}

	if token.AccessToken == "" {
		token.AccessToken = stringValue(metadata, "access_token")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = stringValue(metadata, "refresh_token")
	}
	if token.TokenType == "" {
		token.TokenType = stringValue(metadata, "token_type")
	}
	if token.Expiry.IsZero() {
		if expiry := stringValue(metadata, "expiry"); expiry != "" {
			if ts, errParseTime := time.Parse(time.RFC3339, expiry); errParseTime == nil {
				token.Expiry = ts
			}
		}
	}

	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	clientID, clientSecret := geminiAuth.ResolveOAuthClientCredentials(cfg, base, metadata)
	if strings.TrimSpace(clientID) == "" {
		return "", fmt.Errorf("gemini oauth client-id missing (set config oauth-clients.gemini.client-id or env %s)", config.EnvGeminiOAuthClientID)
	}
	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       geminiOAuthScopes,
		Endpoint:     google.Endpoint,
	}

	ctxToken := ctx
	httpClient := util.NewHTTPClient(defaultAPICallTimeout)
	httpClient.Transport = h.apiCallTransport(auth)
	ctxToken = context.WithValue(ctxToken, oauth2.HTTPClient, httpClient)

	src := conf.TokenSource(ctxToken, &token)
	currentToken, errToken := src.Token()
	if errToken != nil {
		return "", errToken
	}

	merged := buildOAuthTokenMap(base, currentToken)
	fields := buildOAuthTokenFields(currentToken, merged)
	if updater != nil {
		updater(fields)
	}
	return strings.TrimSpace(currentToken.AccessToken), nil
}

func (h *Handler) refreshAntigravityOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("antigravity oauth metadata missing")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(metadata))
	if current != "" && !antigravityTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("antigravity refresh token missing")
	}

	tokenURL := strings.TrimSpace(antigravityOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	cfg := h.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	clientID, clientSecret := cfg.OAuthClientCredentials(config.OAuthClientAntigravity)
	if strings.TrimSpace(clientID) == "" {
		return "", fmt.Errorf("antigravity oauth client-id missing (set config oauth-clients.antigravity.client-id or env %s)", config.EnvAntigravityOAuthClientID)
	}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := util.NewHTTPClient(defaultAPICallTimeout)
	httpClient.Transport = h.apiCallTransport(auth)
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := bodyutil.ReadAll(resp.Body, managementOAuthTokenResponseLimit)
	if errRead != nil {
		if bodyutil.IsTooLarge(errRead) {
			return "", fmt.Errorf("antigravity oauth token refresh response too large")
		}
		return "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("antigravity oauth token refresh failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return "", errUnmarshal
	}

	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return "", fmt.Errorf("antigravity oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
	if strings.TrimSpace(tokenResp.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenResp.RefreshToken)
	}
	if tokenResp.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = tokenResp.ExpiresIn
		auth.Metadata["timestamp"] = now.UnixMilli()
		auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	auth.Metadata["type"] = "antigravity"

	if h != nil && h.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		_, _ = h.authManager.Update(ctx, auth)
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func (h *Handler) refreshKimiOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("kimi oauth metadata missing")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(metadata))
	expStr := stringValue(metadata, "expired")
	if current != "" && expStr != "" {
		if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
			if time.Now().Add(30 * time.Second).Before(ts) {
				return current, nil
			}
		}
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("kimi refresh token missing")
	}

	deviceID := stringValue(metadata, "device_id")

	form := url.Values{}
	form.Set("client_id", kimiOAuthClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, kimiOAuthTokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Msh-Platform", "cli-proxy-api")
	if deviceID != "" {
		req.Header.Set("X-Msh-Device-Id", deviceID)
	}

	httpClient := util.NewHTTPClient(30 * time.Second)
	httpClient.Transport = h.apiCallTransport(auth)
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := bodyutil.ReadAll(resp.Body, managementOAuthTokenResponseLimit)
	if errRead != nil {
		if bodyutil.IsTooLarge(errRead) {
			return "", fmt.Errorf("kimi oauth token refresh response too large")
		}
		return "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("kimi oauth token refresh failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var tokenResp struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		ExpiresIn    float64 `json:"expires_in"`
		TokenType    string  `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return "", errUnmarshal
	}

	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return "", fmt.Errorf("kimi oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
	if strings.TrimSpace(tokenResp.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenResp.RefreshToken)
	}
	auth.Metadata["type"] = "kimi"
	if deviceID != "" {
		auth.Metadata["device_id"] = deviceID
	}
	if tokenResp.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = int64(tokenResp.ExpiresIn)
		auth.Metadata["timestamp"] = now.UnixMilli()
		auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}

	if h != nil && h.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		_, _ = h.authManager.Update(ctx, auth)
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
	// Refresh a bit early to avoid requests racing token expiry.
	const skew = 30 * time.Second

	if metadata == nil {
		return true
	}
	if expStr, ok := metadata["expired"].(string); ok {
		if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
			return !ts.After(time.Now().Add(skew))
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(skew))
	}
	return true
}

func claudeTokenNeedsRefresh(metadata map[string]any) bool {
	// Refresh a bit early to avoid requests racing token expiry.
	const skew = 30 * time.Second

	if metadata == nil {
		return true
	}
	for _, key := range []string{"expired", "expiry", "expires_at", "expiresAt"} {
		if expStr, ok := metadata[key].(string); ok {
			if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
				return !ts.After(time.Now().Add(skew))
			}
		}
	}
	return false
}

func int64Value(raw any) int64 {
	switch typed := raw.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0
		}
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i
		}
	case string:
		if s := strings.TrimSpace(typed); s != "" {
			if i, errParse := json.Number(s).Int64(); errParse == nil {
				return i
			}
		}
	}
	return 0
}

func geminiOAuthMetadata(auth *coreauth.Auth) (map[string]any, func(map[string]any)) {
	if auth == nil {
		return nil, nil
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		snapshot := shared.MetadataSnapshot()
		return snapshot, func(fields map[string]any) { shared.MergeMetadata(fields) }
	}
	return auth.Metadata, func(fields map[string]any) {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		for k, v := range fields {
			auth.Metadata[k] = v
		}
	}
}

func stringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 || key == "" {
		return ""
	}
	if v, ok := metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func buildOAuthTokenMap(base map[string]any, tok *oauth2.Token) map[string]any {
	merged := cloneMap(base)
	if merged == nil {
		merged = make(map[string]any)
	}
	if tok == nil {
		return merged
	}
	if raw, errMarshal := json.Marshal(tok); errMarshal == nil {
		var tokenMap map[string]any
		if errUnmarshal := json.Unmarshal(raw, &tokenMap); errUnmarshal == nil {
			for k, v := range tokenMap {
				merged[k] = v
			}
		}
	}
	return merged
}

func buildOAuthTokenFields(tok *oauth2.Token, merged map[string]any) map[string]any {
	fields := make(map[string]any, 5)
	if tok != nil && tok.AccessToken != "" {
		fields["access_token"] = tok.AccessToken
	}
	if tok != nil && tok.TokenType != "" {
		fields["token_type"] = tok.TokenType
	}
	if tok != nil && tok.RefreshToken != "" {
		fields["refresh_token"] = tok.RefreshToken
	}
	if tok != nil && !tok.Expiry.IsZero() {
		fields["expiry"] = tok.Expiry.Format(time.RFC3339)
	}
	if len(merged) > 0 {
		fields["token"] = cloneMap(merged)
	}
	return fields
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok && tokenRaw != nil {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, ok := typed["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]string:
			if v := strings.TrimSpace(typed["access_token"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(typed["accessToken"]); v != "" {
				return v
			}
		}
	}
	if v, ok := metadata["token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["id_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["cookie"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

func (h *Handler) apiCallTransport(auth *coreauth.Auth) http.RoundTripper {
	var proxyCandidates []string
	if h != nil && h.cfg != nil {
		proxyID := ""
		fallbackURL := ""
		if auth != nil {
			proxyID = auth.ProxyID
			fallbackURL = auth.ProxyURL
		}
		if proxyStr := strings.TrimSpace(h.cfg.ResolveProxyURL(proxyID, fallbackURL)); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	} else if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}

	for _, proxyStr := range proxyCandidates {
		if transport := buildProxyTransport(proxyStr); transport != nil {
			return transport
		}
	}

	return nil
}

func buildProxyTransport(proxyStr string) *http.Transport {
	proxyStr = strings.TrimSpace(proxyStr)
	if proxyStr == "" {
		return nil
	}

	proxyURL, errParse := url.Parse(proxyStr)
	if errParse != nil {
		log.WithError(errParse).Debug("parse proxy URL failed")
		return nil
	}
	if proxyURL.Scheme == "" || proxyURL.Host == "" {
		log.Debug("proxy URL missing scheme/host")
		return nil
	}

	if proxyURL.Scheme == "socks5" {
		var proxyAuth *proxy.Auth
		if proxyURL.User != nil {
			username := proxyURL.User.Username()
			password, _ := proxyURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", proxyURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.WithError(errSOCKS5).Debug("create SOCKS5 dialer failed")
			return nil
		}
		return &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
	}

	if proxyURL.Scheme == "http" || proxyURL.Scheme == "https" {
		return &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	}

	log.Debugf("unsupported proxy scheme: %s", proxyURL.Scheme)
	return nil
}

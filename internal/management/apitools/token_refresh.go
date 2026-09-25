package apitools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/gemini"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/geminicli"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

func TokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := TokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		if v := TokenValueFromMetadata(shared.MetadataSnapshot()); v != "" {
			return v
		}
	}
	return ""
}

func (s *Service) ResolveTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "gemini-cli":
		return s.refreshGeminiOAuthAccessToken(ctx, auth)
	case "antigravity":
		return s.coordinatedRefresh(ctx, auth, antigravityTokenNeedsRefresh, s.refreshAntigravityOAuthAccessToken)
	case "claude", "anthropic":
		return s.coordinatedRefresh(ctx, auth, claudeTokenNeedsRefresh, s.refreshClaudeOAuthAccessToken)
	case "kimi":
		return s.coordinatedRefresh(ctx, auth, kimiTokenNeedsRefresh, s.refreshKimiOAuthAccessToken)
	case "xai", "x-ai", "grok":
		return s.coordinatedRefresh(ctx, auth, xaiTokenNeedsRefresh, s.refreshXAIOAuthAccessToken)
	default:
		return TokenValueForAuth(auth), nil
	}
}

// refreshXAIOAuthAccessToken keeps management api-call / quota probes usable after
// cross-tenant OAuth file import. Without refresh, expired access tokens surface as
// "错误 / --" on auth-file cards even when refresh_token is still valid.
func (s *Service) refreshXAIOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	// API-key credentials never go through the OAuth refresh endpoint.
	if authKind := strings.ToLower(stringValue(auth.Metadata, "auth_kind")); authKind == "api_key" {
		return TokenValueForAuth(auth), nil
	}
	if apiKey := strings.TrimSpace(TokenValueForAuth(auth)); apiKey != "" {
		if stringValue(auth.Metadata, "refresh_token") == "" && stringValue(auth.Metadata, "access_token") == "" {
			return apiKey, nil
		}
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("xai oauth metadata missing")
	}

	current := strings.TrimSpace(TokenValueFromMetadata(metadata))
	if current != "" && !xaiTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		if current != "" {
			return current, nil
		}
		return "", fmt.Errorf("xai refresh token missing")
	}

	tokenEndpoint := stringValue(metadata, "token_endpoint")
	cfg := s.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	svc := xaiauth.NewXAIAuthWithProxyURL(cfg, auth.ProxyURL)
	tokenData, errRefresh := svc.RefreshTokens(ctx, refreshToken, tokenEndpoint)
	if errRefresh != nil {
		return "", errRefresh
	}
	if tokenData == nil || strings.TrimSpace(tokenData.AccessToken) == "" {
		return "", fmt.Errorf("xai oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["type"] = "xai"
	auth.Metadata["auth_kind"] = "oauth"
	auth.Metadata["access_token"] = strings.TrimSpace(tokenData.AccessToken)
	if strings.TrimSpace(tokenData.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenData.RefreshToken)
	}
	if strings.TrimSpace(tokenData.IDToken) != "" {
		auth.Metadata["id_token"] = strings.TrimSpace(tokenData.IDToken)
	}
	if strings.TrimSpace(tokenData.TokenType) != "" {
		auth.Metadata["token_type"] = strings.TrimSpace(tokenData.TokenType)
	}
	if tokenData.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = tokenData.ExpiresIn
	}
	if strings.TrimSpace(tokenData.Expire) != "" {
		auth.Metadata["expired"] = strings.TrimSpace(tokenData.Expire)
	}
	if strings.TrimSpace(tokenData.Email) != "" {
		auth.Metadata["email"] = strings.TrimSpace(tokenData.Email)
	}
	if strings.TrimSpace(tokenData.Subject) != "" {
		auth.Metadata["sub"] = strings.TrimSpace(tokenData.Subject)
	}
	if tokenEndpoint != "" {
		auth.Metadata["token_endpoint"] = tokenEndpoint
	}
	if stringValue(auth.Metadata, "base_url") == "" {
		auth.Metadata["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	auth.Metadata["last_refresh"] = now.UTC().Format(time.RFC3339)
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes["auth_kind"] = "oauth"
	if strings.TrimSpace(auth.Attributes["base_url"]) == "" {
		auth.Attributes["base_url"] = xaiauth.DefaultAPIBaseURL
	}
	if email := stringValue(auth.Metadata, "email"); email != "" {
		auth.Attributes["email"] = email
	}

	if s != nil && s.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		s.persistRefreshedAuth(ctx, auth)
	}

	return strings.TrimSpace(tokenData.AccessToken), nil
}

func xaiTokenNeedsRefresh(metadata map[string]any) bool {
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
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(skew))
	}
	// No parseable expiry: keep current access_token (same as claude). Refresh only
	// when the probe fails or metadata explicitly marks the token expired.
	return false
}

func (s *Service) refreshClaudeOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
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

	current := strings.TrimSpace(TokenValueFromMetadata(metadata))
	if current != "" && !claudeTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("claude refresh token missing")
	}

	refresherFactory := s.deps.NewClaudeOAuthRefresher
	if refresherFactory == nil {
		refresherFactory = func(cfg *config.Config) ClaudeOAuthRefresher {
			if cfg == nil {
				cfg = &config.Config{}
			}
			return claudeauth.NewClaudeAuth(cfg)
		}
	}
	refresher := refresherFactory(s.claudeOAuthRefreshConfig(auth))
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

	if s != nil && s.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		s.persistRefreshedAuth(ctx, auth)
	}

	return strings.TrimSpace(tokenData.AccessToken), nil
}

func (s *Service) claudeOAuthRefreshConfig(auth *coreauth.Auth) *config.Config {
	var cfgCopy config.Config
	if s != nil && s.cfg != nil {
		cfgCopy = *s.cfg
	}
	if auth == nil {
		return &cfgCopy
	}

	var proxyURL string
	if s != nil && s.cfg != nil {
		proxyURL = s.cfg.ResolveProxyURL(auth.ProxyID, auth.ProxyURL)
	} else {
		proxyURL = auth.ProxyURL
	}
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		cfgCopy.ProxyURL = trimmed
	}
	return &cfgCopy
}

func (s *Service) refreshGeminiOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
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

	cfg := s.cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	clientID, clientSecret := geminiAuth.ResolveOAuthClientCredentials(cfg, base, metadata)
	if strings.TrimSpace(clientID) == "" {
		return "", fmt.Errorf("gemini oauth client-id missing (set config oauth-clients.gemini.client-id or env %s)", config.EnvGeminiOAuthClientID)
	}
	scopes := append([]string(nil), s.deps.GeminiOAuthScopes...)
	if len(scopes) == 0 {
		scopes = []string{
			"https://www.googleapis.com/auth/cloud-platform",
			"https://www.googleapis.com/auth/userinfo.email",
			"https://www.googleapis.com/auth/userinfo.profile",
		}
	}
	conf := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Scopes:       scopes,
		Endpoint:     google.Endpoint,
	}

	ctxToken := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Timeout:   s.defaultAPICallTimeout(),
		Transport: s.APICallTransport(auth),
	})

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

func (s *Service) refreshAntigravityOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
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

	current := strings.TrimSpace(TokenValueFromMetadata(metadata))
	if current != "" && !antigravityTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("antigravity refresh token missing")
	}

	tokenURL := strings.TrimSpace(s.deps.AntigravityOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	cfg := s.cfg
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

	httpClient := util.NewHTTPClient(s.defaultAPICallTimeout())
	httpClient.Transport = s.APICallTransport(auth)
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := bodyutil.ReadAll(resp.Body, s.oauthTokenResponseLimit())
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

	if s != nil && s.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		s.persistRefreshedAuth(ctx, auth)
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func (s *Service) refreshKimiOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
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

	current := strings.TrimSpace(TokenValueFromMetadata(metadata))
	if current != "" && !kimiTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("kimi refresh token missing")
	}

	deviceID := stringValue(metadata, "device_id")

	form := url.Values{}
	clientID := s.deps.KimiOAuthClientID
	if strings.TrimSpace(clientID) == "" {
		clientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	tokenURL := s.deps.KimiOAuthTokenURL
	if strings.TrimSpace(tokenURL) == "" {
		tokenURL = "https://auth.kimi.com/api/oauth/token"
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
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
	httpClient.Transport = s.APICallTransport(auth)
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := bodyutil.ReadAll(resp.Body, s.oauthTokenResponseLimit())
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

	if s != nil && s.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		s.persistRefreshedAuth(ctx, auth)
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
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

func TokenValueFromMetadata(metadata map[string]any) string {
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

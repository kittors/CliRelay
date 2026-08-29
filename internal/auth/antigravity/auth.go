// Package antigravity provides OAuth2 authentication functionality for the Antigravity provider.
package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// TokenResponse represents OAuth token response from Google
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// userInfo represents Google user profile
type userInfo struct {
	Email string `json:"email"`
}

// AntigravityAuth handles Antigravity OAuth authentication
type AntigravityAuth struct {
	httpClient *http.Client
	clientID   string
	secret     string
}

// NewAntigravityAuthWithProxy is NewAntigravityAuth pinned to one egress proxy.
// An empty proxyURL keeps the configured default.
func NewAntigravityAuthWithProxy(cfg *config.Config, proxyURL string) *AntigravityAuth {
	if cfg == nil {
		cfg = &config.Config{}
	}
	sdkCfg := cfg.SDKConfig
	if trimmed := strings.TrimSpace(proxyURL); trimmed != "" {
		sdkCfg.ProxyURL = trimmed
	}
	clientID, clientSecret := cfg.OAuthClientCredentials(config.OAuthClientAntigravity)
	return &AntigravityAuth{
		httpClient: util.SetProxy(&sdkCfg, util.NewHTTPClient(util.DefaultHTTPClientTimeout)),
		clientID:   clientID,
		secret:     clientSecret,
	}
}

// NewAntigravityAuth creates a new Antigravity auth service.
func NewAntigravityAuth(cfg *config.Config, httpClient *http.Client) *AntigravityAuth {
	if cfg == nil {
		cfg = &config.Config{}
	}
	clientID, clientSecret := cfg.OAuthClientCredentials(config.OAuthClientAntigravity)
	if httpClient != nil {
		return &AntigravityAuth{httpClient: httpClient, clientID: clientID, secret: clientSecret}
	}
	return &AntigravityAuth{
		httpClient: util.SetProxy(&cfg.SDKConfig, util.NewHTTPClient(util.DefaultHTTPClientTimeout)),
		clientID:   clientID,
		secret:     clientSecret,
	}
}

// BuildAuthURL generates the OAuth authorization URL.
func (o *AntigravityAuth) BuildAuthURL(state, redirectURI string) string {
	if strings.TrimSpace(redirectURI) == "" {
		redirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", CallbackPort)
	}
	clientID := strings.TrimSpace(o.clientID)
	if clientID == "" {
		return ""
	}
	params := url.Values{}
	params.Set("access_type", "offline")
	params.Set("client_id", clientID)
	params.Set("prompt", "consent")
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(Scopes, " "))
	params.Set("state", state)
	return AuthEndpoint + "?" + params.Encode()
}

// ExchangeCodeForTokens exchanges authorization code for access and refresh tokens
func (o *AntigravityAuth) ExchangeCodeForTokens(ctx context.Context, code, redirectURI string) (*TokenResponse, error) {
	clientID := strings.TrimSpace(o.clientID)
	if clientID == "" {
		return nil, fmt.Errorf("antigravity token exchange: missing oauth client-id (set config oauth-clients.antigravity.client-id or env %s)", config.EnvAntigravityOAuthClientID)
	}
	data := url.Values{}
	data.Set("code", code)
	data.Set("client_id", clientID)
	data.Set("client_secret", strings.TrimSpace(o.secret))
	data.Set("redirect_uri", redirectURI)
	data.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("antigravity token exchange: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("antigravity token exchange: execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity token exchange: close body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if errRead != nil {
			return nil, fmt.Errorf("antigravity token exchange: read response: %w", errRead)
		}
		body := strings.TrimSpace(string(bodyBytes))
		if body == "" {
			return nil, fmt.Errorf("antigravity token exchange: request failed: status %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("antigravity token exchange: request failed: status %d: %s", resp.StatusCode, body)
	}

	var token TokenResponse
	if errDecode := json.NewDecoder(resp.Body).Decode(&token); errDecode != nil {
		return nil, fmt.Errorf("antigravity token exchange: decode response: %w", errDecode)
	}
	return &token, nil
}

// FetchUserInfo retrieves user email from Google
func (o *AntigravityAuth) FetchUserInfo(ctx context.Context, accessToken string) (string, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", fmt.Errorf("antigravity userinfo: missing access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoEndpoint, nil)
	if err != nil {
		return "", fmt.Errorf("antigravity userinfo: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return "", fmt.Errorf("antigravity userinfo: execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity userinfo: close body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, errRead := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		if errRead != nil {
			return "", fmt.Errorf("antigravity userinfo: read response: %w", errRead)
		}
		body := strings.TrimSpace(string(bodyBytes))
		if body == "" {
			return "", fmt.Errorf("antigravity userinfo: request failed: status %d", resp.StatusCode)
		}
		return "", fmt.Errorf("antigravity userinfo: request failed: status %d: %s", resp.StatusCode, body)
	}
	var info userInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		return "", fmt.Errorf("antigravity userinfo: decode response: %w", errDecode)
	}
	email := strings.TrimSpace(info.Email)
	if email == "" {
		return "", fmt.Errorf("antigravity userinfo: response missing email")
	}
	return email, nil
}

// FetchAccountDetails retrieves the project ID and plan type via loadCodeAssist
func (o *AntigravityAuth) FetchAccountDetails(ctx context.Context, accessToken string) (AccountInfo, error) {
	loadReqBody := map[string]any{
		"metadata": map[string]string{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	rawBody, errMarshal := json.Marshal(loadReqBody)
	if errMarshal != nil {
		return AccountInfo{}, fmt.Errorf("marshal request body: %w", errMarshal)
	}

	endpointURL := fmt.Sprintf("%s/%s:loadCodeAssist", APIEndpoint, APIVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, strings.NewReader(string(rawBody)))
	if err != nil {
		return AccountInfo{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", APIUserAgent)
	req.Header.Set("X-Goog-Api-Client", APIClient)
	req.Header.Set("Client-Metadata", ClientMetadata)

	resp, errDo := o.httpClient.Do(req)
	if errDo != nil {
		return AccountInfo{}, fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("antigravity loadCodeAssist: close body error: %v", errClose)
		}
	}()

	bodyBytes, errRead := util.ReadHTTPResponseBody("antigravity-oauth", resp.Body)
	if errRead != nil {
		return AccountInfo{}, fmt.Errorf("read response: %w", errRead)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return AccountInfo{}, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var loadResp map[string]any
	if errDecode := json.Unmarshal(bodyBytes, &loadResp); errDecode != nil {
		return AccountInfo{}, fmt.Errorf("decode response: %w", errDecode)
	}

	// Extract projectID and plan details
	projectID := ""
	if id, ok := loadResp["cloudaicompanionProject"].(string); ok {
		projectID = strings.TrimSpace(id)
	}
	if projectID == "" {
		if projectMap, ok := loadResp["cloudaicompanionProject"].(map[string]any); ok {
			if id, okID := projectMap["id"].(string); okID {
				projectID = strings.TrimSpace(id)
			}
		}
	}

	planType := ""
	tierID := ""
	if paidTier, ok := loadResp["paidTier"].(map[string]any); ok {
		if name, okName := paidTier["name"].(string); okName && strings.TrimSpace(name) != "" {
			planType = strings.TrimSpace(name)
		} else if id, okID := paidTier["id"].(string); okID {
			planType = strings.TrimSpace(id)
		}
	}
	if currentTier, ok := loadResp["currentTier"].(map[string]any); ok {
		if id, okID := currentTier["id"].(string); okID {
			tierID = strings.TrimSpace(id)
		}
		if planType == "" {
			if name, okName := currentTier["name"].(string); okName && strings.TrimSpace(name) != "" {
				planType = strings.TrimSpace(name)
			} else {
				planType = tierID
			}
		}
	}

	if projectID == "" {
		defaultTierID := "legacy-tier"
		if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
			for _, rawTier := range tiers {
				tier, okTier := rawTier.(map[string]any)
				if !okTier {
					continue
				}
				if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
					if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
						defaultTierID = strings.TrimSpace(id)
						break
					}
				}
			}
		}

		onboardedProject, err := o.OnboardUser(ctx, accessToken, defaultTierID)
		if err != nil {
			return AccountInfo{PlanType: planType, TierID: tierID}, err
		}
		projectID = onboardedProject
	}

	return AccountInfo{
		ProjectID: projectID,
		PlanType:  planType,
		TierID:    tierID,
	}, nil
}

// FetchProjectID retrieves the project ID for the authenticated user via loadCodeAssist
func (o *AntigravityAuth) FetchProjectID(ctx context.Context, accessToken string) (string, error) {
	info, err := o.FetchAccountDetails(ctx, accessToken)
	if err != nil {
		return "", err
	}
	return info.ProjectID, nil
}

// OnboardUser attempts to fetch the project ID via onboardUser by polling for completion
func (o *AntigravityAuth) OnboardUser(ctx context.Context, accessToken, tierID string) (string, error) {
	log.Infof("Antigravity: onboarding user with tier: %s", tierID)
	requestBody := map[string]any{
		"tierId": tierID,
		"metadata": map[string]string{
			"ideType":    "ANTIGRAVITY",
			"platform":   "PLATFORM_UNSPECIFIED",
			"pluginType": "GEMINI",
		},
	}

	rawBody, errMarshal := json.Marshal(requestBody)
	if errMarshal != nil {
		return "", fmt.Errorf("marshal request body: %w", errMarshal)
	}

	maxAttempts := 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		log.Debugf("Polling attempt %d/%d", attempt, maxAttempts)

		reqCtx := ctx
		var cancel context.CancelFunc
		if reqCtx == nil {
			reqCtx = context.Background()
		}
		reqCtx, cancel = context.WithTimeout(reqCtx, 30*time.Second)

		endpointURL := fmt.Sprintf("%s/%s:onboardUser", APIEndpoint, APIVersion)
		req, errRequest := http.NewRequestWithContext(reqCtx, http.MethodPost, endpointURL, strings.NewReader(string(rawBody)))
		if errRequest != nil {
			cancel()
			return "", fmt.Errorf("create request: %w", errRequest)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", APIUserAgent)
		req.Header.Set("X-Goog-Api-Client", APIClient)
		req.Header.Set("Client-Metadata", ClientMetadata)

		resp, errDo := o.httpClient.Do(req)
		if errDo != nil {
			cancel()
			return "", fmt.Errorf("execute request: %w", errDo)
		}

		bodyBytes, errRead := util.ReadHTTPResponseBody("antigravity-oauth", resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("close body error: %v", errClose)
		}
		cancel()

		if errRead != nil {
			return "", fmt.Errorf("read response: %w", errRead)
		}

		if resp.StatusCode == http.StatusOK {
			var data map[string]any
			if errDecode := json.Unmarshal(bodyBytes, &data); errDecode != nil {
				return "", fmt.Errorf("decode response: %w", errDecode)
			}

			if done, okDone := data["done"].(bool); okDone && done {
				projectID := ""
				if responseData, okResp := data["response"].(map[string]any); okResp {
					switch projectValue := responseData["cloudaicompanionProject"].(type) {
					case map[string]any:
						if id, okID := projectValue["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					case string:
						projectID = strings.TrimSpace(projectValue)
					}
				}

				if projectID != "" {
					log.Infof("Successfully fetched project_id: %s", projectID)
					return projectID, nil
				}

				return "", fmt.Errorf("no project_id in response")
			}

			time.Sleep(2 * time.Second)
			continue
		}

		responsePreview := strings.TrimSpace(string(bodyBytes))
		if len(responsePreview) > 500 {
			responsePreview = responsePreview[:500]
		}

		responseErr := responsePreview
		if len(responseErr) > 200 {
			responseErr = responseErr[:200]
		}
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, responseErr)
	}

	return "", nil
}

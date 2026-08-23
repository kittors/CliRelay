package executor

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ProbeQuotaRecovery checks Grok Build's weekly billing view without consuming a
// user inference request.
func (e *XAIExecutor) ProbeQuotaRecovery(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.QuotaProbeResult, error) {
	if auth == nil {
		return nil, fmt.Errorf("xai executor: auth is nil")
	}
	if xaiUsingAPI(auth) {
		return nil, fmt.Errorf("xai executor: quota recovery probe requires Grok Build OAuth")
	}
	token, _ := xaiCreds(auth)
	if token == "" {
		return nil, fmt.Errorf("xai executor: missing access token")
	}

	endpoint := strings.TrimRight(xaiChatBaseURL(auth), "/") + xaiauth.BillingWeeklyPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	applyXAIChatHeaders(req, e.cfg, auth, token, false)
	if userID := xaiQuotaProbeUserID(auth); userID != "" {
		req.Header.Set("X-UserID", userID)
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close quota probe body error: %v", errClose)
		}
	}()

	body, err := readUpstreamResponseBody(e.Identifier(), resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newXAIStatusErr(resp.StatusCode, body, resp.Header)
	}
	result := parseXAIQuotaProbe(body, time.Now())
	if result == nil {
		return nil, fmt.Errorf("xai executor: invalid weekly billing response")
	}
	return result, nil
}

func parseXAIQuotaProbe(body []byte, now time.Time) *cliproxyauth.QuotaProbeResult {
	weekly, ok := xaiauth.ParseWeeklyBilling(body)
	if !ok {
		return nil
	}
	// Exhaustion is a threshold, not "> 0": weekly percentages carry upstream
	// precision now, and a sliver of remaining allowance cannot serve a request.
	if !weekly.Exhausted() {
		return &cliproxyauth.QuotaProbeResult{Recovered: true}
	}
	result := &cliproxyauth.QuotaProbeResult{
		Recovered:       false,
		WindowExhausted: true,
		Window:          xaiWeeklyWindowLabel,
		WindowMinutes:   xaiWeeklyWindowMinutes,
	}
	if weekly.ResetAt.After(now) {
		result.NextRecoverAt = weekly.ResetAt
	}
	return result
}

func xaiQuotaProbeUserID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"sub", "subject", "user_id", "userId", "x_userid"} {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
		if value := xaiMetadataString(auth.Metadata, key); value != "" {
			return value
		}
	}
	for _, parent := range []string{"oauth", "user"} {
		raw, ok := auth.Metadata[parent]
		if !ok {
			continue
		}
		nested, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"sub", "subject", "id", "user_id", "userId"} {
			if value := xaiMetadataString(nested, key); value != "" {
				return value
			}
		}
	}
	return ""
}

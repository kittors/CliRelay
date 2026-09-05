package apitools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var errBlockedAPICallTarget = errors.New("blocked api-call target")

var blockedAPICallPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func (s *Service) APICall(ctx context.Context, body APICallRequest) (int, any) {
	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		return http.StatusBadRequest, map[string]any{"error": "missing method"}
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		return http.StatusBadRequest, map[string]any{"error": "missing url"}
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return http.StatusBadRequest, map[string]any{"error": "invalid url"}
	}
	if errValidateURL := validateAPICallURL(ctx, parsedURL); errValidateURL != nil {
		return http.StatusBadRequest, map[string]any{"error": "blocked url"}
	}

	authIndex := FirstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := s.AuthByIndex(authIndex)

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
			token, tokenErr = s.ResolveTokenForAuth(ctx, auth)
			tokenResolved = true
		}
		if auth != nil && token == "" {
			if tokenErr != nil {
				return http.StatusBadRequest, map[string]any{"error": "auth token refresh failed"}
			}
			return http.StatusBadRequest, map[string]any{"error": "auth token not found"}
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

	req, errNewRequest := http.NewRequestWithContext(ctx, method, urlStr, requestBody)
	if errNewRequest != nil {
		return http.StatusBadRequest, map[string]any{"error": "failed to build request"}
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

	httpClient := util.NewHTTPClient(s.defaultAPICallTimeout())
	httpClient.Transport = s.APICallTransportForURL(auth, parsedURL)
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return validateAPICallURL(req.Context(), req.URL)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		if errors.Is(errDo, errBlockedAPICallTarget) {
			return http.StatusBadRequest, map[string]any{"error": "blocked url"}
		}
		log.WithError(errDo).Debug("management APICall request failed")
		return http.StatusBadGateway, map[string]any{"error": "request failed"}
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := bodyutil.ReadAll(resp.Body, s.apiCallResponseLimit())
	if errReadAll != nil {
		if bodyutil.IsTooLarge(errReadAll) {
			return http.StatusBadGateway, map[string]any{"error": "upstream response too large"}
		}
		return http.StatusBadGateway, map[string]any{"error": "failed to read response"}
	}
	if errReconcile := s.ReconcileCodexWhamUsagePlan(ctx, auth, parsedURL, resp.StatusCode, respBody); errReconcile != nil {
		log.WithError(errReconcile).Warn("failed to reconcile codex usage plan type")
	}
	if isCodexResetConsumeURL(parsedURL) && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if s.authManager != nil && auth != nil {
			_, _ = s.authManager.ClearQuotaStatus(ctx, auth.ID)
		}
	}

	return http.StatusOK, APICallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       string(respBody),
	}
}

func validateAPICallURL(ctx context.Context, parsedURL *url.URL) error {
	if parsedURL == nil {
		return errBlockedAPICallTarget
	}
	scheme := strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if scheme != "http" && scheme != "https" {
		return errBlockedAPICallTarget
	}
	host := strings.TrimSpace(parsedURL.Hostname())
	if host == "" {
		return errBlockedAPICallTarget
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if apiCallAddrBlocked(addr) {
			return errBlockedAPICallTarget
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return errBlockedAPICallTarget
	}
	for _, addr := range addrs {
		if apiCallAddrBlocked(addr) {
			return errBlockedAPICallTarget
		}
	}
	return nil
}

func apiCallAddrBlocked(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedAPICallPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func FirstNonEmptyString(values ...*string) string {
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

func (s *Service) ReconcileCodexWhamUsagePlan(ctx context.Context, auth *coreauth.Auth, parsedURL *url.URL, statusCode int, respBody []byte) error {
	if s == nil || s.authManager == nil || auth == nil {
		return nil
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return nil
	}
	if !isCodexWhamUsageURL(parsedURL) {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}

	var payload struct {
		PlanType string `json:"plan_type"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return nil
	}
	planType := managementauthfiles.NormalizeTagValue(payload.PlanType)
	if planType == "" || auth.Metadata == nil {
		return nil
	}

	changed := false
	if currentType, _ := auth.Metadata["type"].(string); strings.TrimSpace(currentType) == "" {
		auth.Metadata["type"] = "codex"
		changed = true
	}
	currentPlanType := managementauthfiles.NormalizeTagValue(managementauthfiles.MetadataString(auth.Metadata, "plan_type", "planType"))
	if currentPlanType != planType {
		auth.Metadata["plan_type"] = planType
		delete(auth.Metadata, "planType")
		changed = true
	}
	if reconcileAuthExplicitDisplayTags(auth) {
		changed = true
	}
	if !changed {
		return nil
	}

	now := time.Now()
	auth.UpdatedAt = now
	_, err := s.authManager.Update(ctx, auth)
	return err
}

func isCodexWhamUsageURL(parsedURL *url.URL) bool {
	if parsedURL == nil {
		return false
	}
	path := strings.TrimRight(parsedURL.EscapedPath(), "/")
	return path == "/backend-api/wham/usage"
}
func isCodexResetConsumeURL(parsedURL *url.URL) bool {
	if parsedURL == nil {
		return false
	}
	path := strings.TrimRight(parsedURL.EscapedPath(), "/")
	return path == "/backend-api/wham/rate-limit-reset-credits/consume"
}

func reconcileAuthExplicitDisplayTags(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	currentTags, ok := managementauthfiles.MetadataStringSliceWithPresence(auth.Metadata, "display_tags")
	if !ok {
		return false
	}
	reconciledTags := managementauthfiles.BuildTagPayload(auth).DisplayTags
	if normalizedStringSlicesEqual(currentTags, reconciledTags) {
		return false
	}
	auth.Metadata["display_tags"] = reconciledTags
	return true
}

func normalizedStringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if managementauthfiles.NormalizeTagValue(a[i]) != managementauthfiles.NormalizeTagValue(b[i]) {
			return false
		}
	}
	return true
}

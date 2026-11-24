package apitools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

var (
	// fakeIPPrefix matches RFC 2544 benchmark space (198.18.0.0/15), commonly
	// hijacked by local TUN/Clash/mihomo Fake-IP DNS resolvers.
	fakeIPPrefix = netip.MustParsePrefix("198.18.0.0/15")

	// apiCallLookupNetIP is overridable in tests so DNS resolution edge cases
	// (Fake-IP, private IP, lookup failures) can be exercised deterministically.
	apiCallLookupNetIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
)

var blockedAPICallPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	fakeIPPrefix,
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

type apiCallURLBlockError struct {
	Code    string
	Message string
	Host    string
	IP      string
	Details map[string]any
}

func (e *apiCallURLBlockError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (s *Service) APICall(ctx context.Context, body APICallRequest) (int, any) {
	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		return http.StatusBadRequest, NewAPIError("missing_method", "缺少 HTTP 请求方法（method），请指定 GET、POST、PUT、PATCH 或 DELETE")
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		return http.StatusBadRequest, NewAPIError("missing_url", "缺少目标请求地址（url），请提供完整的 http:// 或 https:// 地址")
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL == nil || strings.TrimSpace(parsedURL.Scheme) == "" {
		return http.StatusBadRequest, NewAPIError("invalid_url", "目标请求地址（url）格式无效，请提供包含 http:// 或 https:// 协议和有效域名的完整地址")
	}

	// Resolve auth and egress proxy BEFORE validating the URL so that domain
	// targets routed through an explicit HTTP/SOCKS proxy are not falsely
	// blocked by the host's local DNS resolver (e.g. local Fake-IP 198.18.0.0/15).
	authIndex := FirstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := s.AuthByIndex(authIndex)
	hasExplicitProxy := s.HasExplicitProxy(auth)

	if blockErr := validateAPICallURL(ctx, parsedURL, hasExplicitProxy); blockErr != nil {
		logAPICallURLBlocked(blockErr, parsedURL, hasExplicitProxy, false)
		return http.StatusBadRequest, NewAPIError(blockErr.Code, blockErr.Message, blockErr.Details)
	}

	reqHeaders := make(map[string]string, len(body.Header))
	for k, v := range body.Header {
		reqHeaders[k] = v
	}

	var hostOverride string
	var token string
	var tokenResolved bool
	var tokenErr error
	for key, value := range reqHeaders {
		if !strings.Contains(value, "$TOKEN$") {
			continue
		}
		// Never forward a literal "$TOKEN$" placeholder upstream. That happens when the
		// client supplies a stale/wrong auth_index (e.g. cross-tenant cache) and AuthByIndex
		// returns nil — upstream then returns a misleading 401 that looks like credential expiry.
		if auth == nil {
			if strings.TrimSpace(authIndex) == "" {
				return http.StatusBadRequest, NewAPIError("missing_auth_index", "请求头包含 $TOKEN$ 占位符，但未提供账号凭据标识（auth_index），请指定目标账号后重试")
			}
			return http.StatusBadRequest, NewAPIError(
				"auth_not_found",
				fmt.Sprintf("未找到凭据标识为 %q 的账号（可能已被删除、刷新或不属于当前租户），请刷新账号列表后重试", authIndex),
				map[string]any{"auth_index": authIndex},
			)
		}
		if !tokenResolved {
			token, tokenErr = s.ResolveTokenForAuth(ctx, auth)
			tokenResolved = true
		}
		if token == "" {
			if tokenErr != nil {
				reason := SanitizeErrorText(tokenErr.Error())
				log.WithFields(log.Fields{
					"auth_id":    auth.ID,
					"auth_index": authIndex,
					"provider":   auth.Provider,
				}).WithError(tokenErr).Warn("management APICall auth token refresh failed")
				if reason != "" {
					return http.StatusBadRequest, NewAPIError(
						"auth_token_refresh_failed",
						fmt.Sprintf("刷新账号访问令牌（Token）失败（%s），请检查账号授权状态或代理配置，必要时重新登录授权", reason),
					)
				}
				return http.StatusBadRequest, NewAPIError(
					"auth_token_refresh_failed",
					"刷新账号访问令牌（Token）失败，请检查账号授权状态或代理配置，必要时重新登录授权",
				)
			}
			return http.StatusBadRequest, NewAPIError("auth_token_not_found", "该账号凭据中未包含有效的访问令牌（Token），请检查凭据内容是否完整或重新登录导入")
		}
		reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
	}

	var requestBody io.Reader
	if body.Data != "" {
		requestBody = strings.NewReader(body.Data)
	}

	req, errNewRequest := http.NewRequestWithContext(ctx, method, urlStr, requestBody)
	if errNewRequest != nil {
		reason := SanitizeErrorText(errNewRequest.Error())
		return http.StatusBadRequest, NewAPIError(
			"build_request_failed",
			fmt.Sprintf("构造上游 HTTP 请求失败（%s），请检查请求方法、URL 和请求头格式", reason),
		)
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
	httpClient.CheckRedirect = func(redirectReq *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		if blockErr := validateAPICallURL(redirectReq.Context(), redirectReq.URL, hasExplicitProxy); blockErr != nil {
			redirectBlock := &apiCallURLBlockError{
				Code:    "blocked_redirect_target",
				Message: fmt.Sprintf("上游请求发生重定向，但重定向目标地址被安全策略拦截：%s", blockErr.Message),
				Host:    blockErr.Host,
				IP:      blockErr.IP,
				Details: map[string]any{
					"reason_code":   blockErr.Code,
					"redirect_host": redirectReq.URL.Hostname(),
				},
			}
			logAPICallURLBlocked(redirectBlock, redirectReq.URL, hasExplicitProxy, true)
			return redirectBlock
		}
		return nil
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		var blockErr *apiCallURLBlockError
		if errors.As(errDo, &blockErr) {
			return http.StatusBadRequest, NewAPIError(blockErr.Code, blockErr.Message, blockErr.Details)
		}
		code, message := DescribeUpstreamNetworkError(errDo)
		log.WithFields(log.Fields{
			"method":             method,
			"host":               parsedURL.Hostname(),
			"has_explicit_proxy": hasExplicitProxy,
			"error_code":         code,
		}).WithError(errDo).Warn("management APICall upstream request failed")
		return http.StatusBadGateway, NewAPIError(code, message)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	responseLimit := s.apiCallResponseLimit()
	respBody, errReadAll := bodyutil.ReadAll(resp.Body, responseLimit)
	if errReadAll != nil {
		if bodyutil.IsTooLarge(errReadAll) {
			return http.StatusBadGateway, NewAPIError(
				"upstream_response_too_large",
				fmt.Sprintf("上游返回内容过大，已超过服务端允许的 %s 上限，已中止读取", formatAPICallByteLimit(responseLimit)),
				map[string]any{"max_bytes": responseLimit},
			)
		}
		reason := SanitizeErrorText(errReadAll.Error())
		return http.StatusBadGateway, NewAPIError(
			"read_upstream_response_failed",
			fmt.Sprintf("读取上游响应内容失败（%s），可能是连接提前中断，请稍后重试", reason),
		)
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

func logAPICallURLBlocked(blockErr *apiCallURLBlockError, parsedURL *url.URL, hasExplicitProxy, isRedirect bool) {
	if blockErr == nil {
		return
	}
	scheme := ""
	host := blockErr.Host
	if parsedURL != nil {
		scheme = strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
		if host == "" {
			host = strings.TrimSpace(parsedURL.Hostname())
		}
	}
	log.WithFields(log.Fields{
		"code":               blockErr.Code,
		"scheme":             scheme,
		"host":               host,
		"ip":                 blockErr.IP,
		"has_explicit_proxy": hasExplicitProxy,
		"redirect":           isRedirect,
	}).Warn(blockErr.Message)
}

func validateAPICallURL(ctx context.Context, parsedURL *url.URL, hasExplicitProxy bool) *apiCallURLBlockError {
	if parsedURL == nil {
		return &apiCallURLBlockError{
			Code:    "missing_url_host",
			Message: "目标地址缺少有效的主机名或域名",
		}
	}
	scheme := strings.ToLower(strings.TrimSpace(parsedURL.Scheme))
	if scheme != "http" && scheme != "https" {
		return &apiCallURLBlockError{
			Code:    "unsupported_url_scheme",
			Message: fmt.Sprintf("目标地址仅支持 http 或 https 协议，当前协议为 %q", scheme),
			Details: map[string]any{"scheme": scheme},
		}
	}
	host := strings.TrimSpace(parsedURL.Hostname())
	if host == "" {
		return &apiCallURLBlockError{
			Code:    "missing_url_host",
			Message: "目标地址缺少有效的主机名或域名",
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return validateAPICallAddr(host, addr, true)
	}
	lowerHost := strings.ToLower(strings.TrimSuffix(host, "."))
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") {
		return &apiCallURLBlockError{
			Code:    "blocked_private_address",
			Message: fmt.Sprintf("出于安全策略，已拦截对本机或内网/保留网段地址 %q（localhost）的请求", host),
			Host:    host,
			Details: map[string]any{"host": host},
		}
	}
	// When an explicit HTTP/SOCKS egress proxy is configured, the proxy server
	// performs remote DNS resolution for domain targets. Skipping local DNS
	// lookup prevents false blocks caused by host Fake-IP DNS (198.18.0.0/15)
	// or isolated server DNS environments.
	if hasExplicitProxy {
		return nil
	}
	addrs, err := apiCallLookupNetIP(ctx, host)
	if err != nil || len(addrs) == 0 {
		reason := "未返回任何 IP 记录"
		if err != nil {
			if sanitized := SanitizeErrorText(err.Error()); sanitized != "" {
				reason = sanitized
			}
		}
		return &apiCallURLBlockError{
			Code:    "dns_lookup_failed",
			Message: fmt.Sprintf("无法解析目标域名 %q 的 DNS 记录（%s），请检查服务器网络、DNS 配置或为该账号配置可用代理", host, reason),
			Host:    host,
			Details: map[string]any{"host": host},
		}
	}
	for _, addr := range addrs {
		if blockErr := validateAPICallAddr(host, addr, false); blockErr != nil {
			return blockErr
		}
	}
	return nil
}

func validateAPICallAddr(host string, addr netip.Addr, isLiteralIP bool) *apiCallURLBlockError {
	if !addr.IsValid() {
		return &apiCallURLBlockError{
			Code:    "blocked_private_address",
			Message: fmt.Sprintf("出于安全策略，已拦截对无效地址 %q 的请求", host),
			Host:    host,
		}
	}
	unmapped := addr.Unmap()
	if fakeIPPrefix.Contains(unmapped) {
		if isLiteralIP {
			return &apiCallURLBlockError{
				Code:    "blocked_fake_ip_address",
				Message: fmt.Sprintf("目标地址 %q 属于虚拟代理地址段（%s，198.18.0.0/15 Fake-IP 网段），出于 SSRF 安全策略无法直连该地址；请使用目标域名并配置显式 HTTP/SOCKS 代理", host, unmapped.String()),
				Host:    host,
				IP:      unmapped.String(),
				Details: map[string]any{"host": host, "ip": unmapped.String()},
			}
		}
		return &apiCallURLBlockError{
			Code:    "blocked_fake_ip_address",
			Message: fmt.Sprintf("目标域名 %q 当前被本机解析为虚拟代理地址（%s，198.18.0.0/15 Fake-IP 网段），出于 SSRF 安全策略无法直连该地址；请为该账号或全局配置显式 HTTP/SOCKS 代理，或将目标域名加入代理直连规则", host, unmapped.String()),
			Host:    host,
			IP:      unmapped.String(),
			Details: map[string]any{"host": host, "ip": unmapped.String()},
		}
	}
	if apiCallAddrBlocked(unmapped) {
		return &apiCallURLBlockError{
			Code:    "blocked_private_address",
			Message: fmt.Sprintf("出于安全策略，已拦截对本机或内网/保留网段地址 %q（%s）的请求", host, unmapped.String()),
			Host:    host,
			IP:      unmapped.String(),
			Details: map[string]any{"host": host, "ip": unmapped.String()},
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

func formatAPICallByteLimit(limitBytes int64) string {
	const (
		kiB = 1024
		miB = 1024 * kiB
	)
	switch {
	case limitBytes >= miB && limitBytes%miB == 0:
		return fmt.Sprintf("%d MiB", limitBytes/miB)
	case limitBytes >= miB:
		return fmt.Sprintf("%.1f MiB", float64(limitBytes)/float64(miB))
	case limitBytes >= kiB && limitBytes%kiB == 0:
		return fmt.Sprintf("%d KiB", limitBytes/kiB)
	case limitBytes >= kiB:
		return fmt.Sprintf("%.1f KiB", float64(limitBytes)/float64(kiB))
	default:
		return fmt.Sprintf("%d 字节", limitBytes)
	}
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
	// MutateAuth edits auth itself on a single node; in cluster mode it applies
	// the same edit to the newest persisted copy.
	_, err := s.authManager.MutateAuth(ctx, auth, func(target *coreauth.Auth) (bool, error) {
		return applyCodexUsagePlan(target, planType, time.Now()), nil
	})
	return err
}

func applyCodexUsagePlan(auth *coreauth.Auth, planType string, now time.Time) bool {
	if auth.Metadata == nil {
		return false
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
	if changed {
		auth.UpdatedAt = now
	}
	return changed
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

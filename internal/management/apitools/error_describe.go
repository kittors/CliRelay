package apitools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// APIErrorDetail is the structured error payload shared by management API tools
// and handlers. It matches the admin panel's extractApiErrorCode /
// extractApiErrorMessage / extractApiErrorDetails helpers.
type APIErrorDetail struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// APIErrorPayload is the standard JSON error envelope:
// {"error": {"code": "...", "message": "...", "details": {...}}}.
type APIErrorPayload struct {
	Error APIErrorDetail `json:"error"`
}

// NewAPIError builds a structured error envelope with optional details.
func NewAPIError(code, message string, details ...map[string]any) APIErrorPayload {
	var merged map[string]any
	for _, item := range details {
		if len(item) == 0 {
			continue
		}
		if merged == nil {
			merged = make(map[string]any, len(item))
		}
		for k, v := range item {
			if v != nil {
				merged[k] = v
			}
		}
	}
	return APIErrorPayload{
		Error: APIErrorDetail{
			Code:    strings.TrimSpace(code),
			Message: strings.TrimSpace(message),
			Details: merged,
		},
	}
}

var (
	urlUserInfoPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@\s]+)@`)
	bearerTokenPattern = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	apiKeyTokenPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	whitespacePattern  = regexp.MustCompile(`\s+`)
)

// SanitizeErrorText redacts embedded proxy credentials, bearer tokens, and
// API keys from an error message, collapses whitespace, and caps its length so
// user-facing error details remain safe and readable.
func SanitizeErrorText(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	cleaned = urlUserInfoPattern.ReplaceAllString(cleaned, "${1}***@")
	cleaned = bearerTokenPattern.ReplaceAllString(cleaned, "${1}***")
	cleaned = apiKeyTokenPattern.ReplaceAllString(cleaned, "sk-***")
	cleaned = whitespacePattern.ReplaceAllString(cleaned, " ")
	cleaned = strings.TrimSpace(cleaned)
	const maxRunes = 240
	runes := []rune(cleaned)
	if len(runes) > maxRunes {
		cleaned = strings.TrimSpace(string(runes[:maxRunes])) + "..."
	}
	return cleaned
}

// DescribeUpstreamNetworkError classifies an outbound HTTP/network error into
// a stable machine code and a clear, actionable Chinese explanation.
func DescribeUpstreamNetworkError(err error) (code string, message string) {
	if err == nil {
		return "upstream_request_failed", "请求上游服务失败，请检查网络连接与代理配置后重试"
	}

	if errors.Is(err, context.Canceled) {
		return "request_canceled", "上游请求已被取消，请重试"
	}

	sanitized := SanitizeErrorText(err.Error())
	lower := strings.ToLower(sanitized)

	// Detect proxy-specific failures first so proxy timeouts/refusals point operators
	// straight at the egress proxy configuration instead of the upstream service.
	if isProxyTransportError(err, lower) {
		if isTimeoutError(err, lower) {
			return "proxy_timeout", fmt.Sprintf("连接代理服务器超时（%s），请检查代理节点是否可达、延迟是否过高或尝试更换代理", sanitized)
		}
		return "proxy_connection_failed", fmt.Sprintf("通过代理连接上游服务失败（%s），请检查代理地址、端口、认证凭据及代理节点运行状态", sanitized)
	}

	if isTimeoutError(err, lower) {
		return "upstream_timeout", "连接上游服务超时，请检查服务器网络状况、代理延迟或稍后重试"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		host := strings.TrimSpace(dnsErr.Name)
		reason := SanitizeErrorText(dnsErr.Err)
		if reason == "" {
			reason = sanitized
		}
		if host != "" {
			return "dns_lookup_failed", fmt.Sprintf("无法解析上游域名 %q 的 DNS 记录（%s），请检查服务器 DNS 配置或为该账号配置可用代理", host, reason)
		}
		return "dns_lookup_failed", fmt.Sprintf("上游域名 DNS 解析失败（%s），请检查服务器 DNS 配置或为该账号配置可用代理", reason)
	}

	if isTLSError(err, lower) {
		return "upstream_tls_error", fmt.Sprintf("与上游建立 TLS 安全连接失败（%s），请检查服务器系统时间、CA 证书配置或代理是否拦截了 HTTPS 流量", sanitized)
	}

	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(lower, "connection refused") {
		return "connection_refused", "上游或代理服务器拒绝了连接（connection refused），请检查目标地址、端口及代理服务状态"
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe") ||
		strings.Contains(lower, "unexpected eof") {
		return "connection_reset", fmt.Sprintf("与上游服务的连接被意外中断（%s），可能是网络抖动、代理断开或上游主动关闭连接导致，请稍后重试", sanitized)
	}

	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		strings.Contains(lower, "network is unreachable") || strings.Contains(lower, "no route to host") {
		return "network_unreachable", "无法到达上游目标网络或主机，请检查服务器出口路由或代理配置"
	}

	if sanitized == "" {
		return "upstream_request_failed", "请求上游服务失败，请检查网络连接与代理配置后重试"
	}
	return "upstream_request_failed", fmt.Sprintf("请求上游服务失败（%s），请检查网络连接与代理配置后重试", sanitized)
}

func isTimeoutError(err error, lower string) bool {
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return strings.Contains(lower, "i/o timeout") ||
		strings.Contains(lower, "client.timeout") ||
		strings.Contains(lower, "context deadline exceeded")
}

func isProxyTransportError(err error, lower string) bool {
	if strings.Contains(lower, "proxyconnect") ||
		strings.Contains(lower, "socks connect") ||
		strings.Contains(lower, "socks5") ||
		strings.Contains(lower, "socks4") ||
		strings.Contains(lower, "proxy authentication required") ||
		strings.Contains(lower, "bad proxy") ||
		strings.Contains(lower, "invalid proxy") {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		if strings.Contains(strings.ToLower(urlErr.Err.Error()), "proxyconnect") {
			return true
		}
	}
	return false
}

func isTLSError(err error, lower string) bool {
	var (
		unknownAuth x509.UnknownAuthorityError
		certInvalid x509.CertificateInvalidError
		hostErr     x509.HostnameError
		recordErr   tls.RecordHeaderError
	)
	if errors.As(err, &unknownAuth) || errors.As(err, &certInvalid) ||
		errors.As(err, &hostErr) || errors.As(err, &recordErr) {
		return true
	}
	return strings.Contains(lower, "tls:") ||
		strings.Contains(lower, "x509:") ||
		strings.Contains(lower, "certificate")
}

// ExtractUpstreamErrorSummary extracts a concise, user-readable error summary
// from an upstream HTTP response body (JSON or plain text), ignoring HTML pages.
func ExtractUpstreamErrorSummary(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		return ""
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil && parsed != nil {
		if msg := extractJSONErrorMessage(parsed); msg != "" {
			return SanitizeErrorText(msg)
		}
	}
	return SanitizeErrorText(trimmed)
}

func extractJSONErrorMessage(m map[string]any) string {
	if errField, ok := m["error"]; ok {
		switch typed := errField.(type) {
		case string:
			if msg := strings.TrimSpace(typed); msg != "" {
				if extra, ok := m["message"].(string); ok && strings.TrimSpace(extra) != "" && !strings.EqualFold(strings.TrimSpace(extra), msg) {
					return msg + ": " + strings.TrimSpace(extra)
				}
				return msg
			}
		case map[string]any:
			if nested := extractJSONErrorMessage(typed); nested != "" {
				return nested
			}
		}
	}
	for _, key := range []string{"detail", "message", "error_description", "reason", "title"} {
		switch typed := m[key].(type) {
		case string:
			if s := strings.TrimSpace(typed); s != "" {
				return s
			}
		case map[string]any:
			if nested := extractJSONErrorMessage(typed); nested != "" {
				return nested
			}
		}
	}
	return ""
}

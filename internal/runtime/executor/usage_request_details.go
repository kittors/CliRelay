package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func apiKeyFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if value := strings.TrimSpace(contextStringValue(ctx, util.ContextKeyAPIKey)); value != "" {
		return value
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return ""
	}
	if v, exists := ginCtx.Get("apiKey"); exists {
		switch value := v.(type) {
		case string:
			return value
		case fmt.Stringer:
			return value.String()
		default:
			return fmt.Sprintf("%v", value)
		}
	}
	return ""
}

func buildRequestDetailContent(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return ""
	}

	req := ginCtx.Request
	apiRequest, _ := ginCtx.Get(apiRequestKey)
	apiResponse, _ := ginCtx.Get(apiResponseKey)
	clientIP, clientIPSource := requestLogClientIP(ginCtx, req)

	detail := map[string]any{
		"client": map[string]any{
			"ip":                  clientIP,
			"ip_source":           clientIPSource,
			"remote_addr":         req.RemoteAddr,
			"method":              req.Method,
			"url":                 req.URL.String(),
			"path":                req.URL.Path,
			"query":               req.URL.Query(),
			"host":                req.Host,
			"content_length":      req.ContentLength,
			"headers":             cloneHeaderValues(req.Header),
			"fingerprint_headers": extractFingerprintHeaders(req.Header),
		},
		"upstream": map[string]any{
			"request_log": bytesToString(apiRequest),
		},
		"response": map[string]any{
			"upstream_log": bytesToString(apiResponse),
		},
	}
	if egress := requestLogEgressFromContext(ginCtx); len(egress) > 0 {
		detail["egress"] = egress
	}
	if timing := upstreamTimingFromContext(ginCtx); len(timing) > 0 {
		detail["upstream_timing"] = timing
	}
	if diagnostic := diagnostics.FromGin(ginCtx); diagnostic != nil {
		if snapshot := diagnostic.Snapshot(); !snapshot.IsZero() {
			detail["diagnostic"] = snapshot
		}
	}

	data, err := json.Marshal(detail)
	if err != nil {
		return ""
	}
	return string(data)
}

func requestLogClientIP(ginCtx *gin.Context, req *http.Request) (string, string) {
	if ip, source := util.ForwardedClientIP(req); ip != "" {
		return ip, source
	}
	if ginCtx != nil {
		if ip := strings.TrimSpace(ginCtx.ClientIP()); ip != "" {
			return ip, "client_ip"
		}
	}
	if req != nil {
		if ip := util.RemoteAddrIP(req.RemoteAddr); ip != "" {
			return ip, "remote_addr"
		}
	}
	return "", ""
}

func bytesToString(value any) string {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return ""
	}
	return string(data)
}

func requestLogEgressFromContext(ginCtx *gin.Context) map[string]any {
	if ginCtx == nil {
		return nil
	}
	value, exists := ginCtx.Get(requestLogEgressRouteKey)
	if !exists {
		return nil
	}
	route, ok := value.(requestLogEgressRoute)
	if !ok {
		return nil
	}
	return requestLogEgressRouteMap(route)
}

func cloneHeaderValues(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		copied := make([]string, len(values))
		copy(copied, values)
		out[key] = copied
	}
	return out
}

func extractFingerprintHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string)
	for key, values := range headers {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		if normalized == "user-agent" ||
			strings.Contains(normalized, "session") ||
			strings.Contains(normalized, "version") ||
			strings.Contains(normalized, "originator") ||
			strings.Contains(normalized, "codex") ||
			strings.Contains(normalized, "claude") ||
			strings.Contains(normalized, "gemini") ||
			strings.HasPrefix(normalized, "x-") {
			copied := make([]string, len(values))
			copy(copied, values)
			out[key] = copied
		}
	}
	return out
}

func contextStringValue(ctx context.Context, key any) string {
	if ctx == nil {
		return ""
	}
	switch value := ctx.Value(key).(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", value)
	}
}

func firstTokenLatencyMsFromContext(ctx context.Context, requestedAt time.Time) int64 {
	if ctx == nil || requestedAt.IsZero() {
		return 0
	}
	ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if !ok || ginCtx == nil {
		return 0
	}
	value, exists := ginCtx.Get(util.GinKeyFirstResponseAt)
	if !exists {
		return 0
	}
	firstResponseAt, ok := value.(time.Time)
	if !ok || firstResponseAt.IsZero() {
		return 0
	}
	latencyMs := firstResponseAt.Sub(requestedAt).Milliseconds()
	if latencyMs < 0 {
		return 0
	}
	return latencyMs
}

func resolveUsageSource(auth *cliproxyauth.Auth, ctxAPIKey string) string {
	if auth != nil {
		provider := strings.TrimSpace(auth.Provider)
		if strings.EqualFold(provider, "gemini-cli") {
			if id := strings.TrimSpace(auth.ID); id != "" {
				return id
			}
		}
		if strings.EqualFold(provider, "vertex") {
			if auth.Metadata != nil {
				if projectID, ok := auth.Metadata["project_id"].(string); ok {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed
					}
				}
				if project, ok := auth.Metadata["project"].(string); ok {
					if trimmed := strings.TrimSpace(project); trimmed != "" {
						return trimmed
					}
				}
			}
		}
		if _, value := auth.AccountInfo(); value != "" {
			return strings.TrimSpace(value)
		}
		if auth.Metadata != nil {
			if email, ok := auth.Metadata["email"].(string); ok {
				if trimmed := strings.TrimSpace(email); trimmed != "" {
					return trimmed
				}
			}
		}
		if auth.Attributes != nil {
			if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
				return key
			}
		}
	}
	if trimmed := strings.TrimSpace(ctxAPIKey); trimmed != "" {
		return trimmed
	}
	return ""
}

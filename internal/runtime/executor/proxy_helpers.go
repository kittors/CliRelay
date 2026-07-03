package executor

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

var globalWarmManager atomic.Pointer[ProxyWarmManager]

// SetGlobalWarmManager installs the global warm manager used by newProxyAwareHTTPClient.
func SetGlobalWarmManager(m *ProxyWarmManager) {
	globalWarmManager.Store(m)
}

const requestLogEgressRouteKey = "cliproxy.request_log.egress_route"

type requestLogEgressRoute struct {
	RouteKind    string `json:"route_kind,omitempty"`
	ProxySource  string `json:"proxy_source,omitempty"`
	ProxyID      string `json:"proxy_id,omitempty"`
	ProxyName    string `json:"proxy_name,omitempty"`
	ProxyURLHost string `json:"proxy_url_host,omitempty"`
}

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyID when it resolves to an enabled proxy-pool entry
// 2. Use auth.ProxyURL if configured
// 3. Use cfg.ProxyURL if auth proxy is not configured
// 4. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := util.NewHTTPClient(timeout)

	var proxyURL string
	if cfg != nil {
		proxyID := ""
		fallbackURL := ""
		if auth != nil {
			proxyID = auth.ProxyID
			fallbackURL = auth.ProxyURL
		}
		proxyURL = cfg.ResolveProxyURL(proxyID, fallbackURL)
	} else if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	recordRequestLogEgressRoute(ctx, cfg, auth, proxyURL)

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := cachedProxyTransport(proxyURL, cfgToSDKCfg(cfg))
		if transport != nil {
			httpClient.Transport = transport
		} else {
			// If proxy setup failed, log and fall through to context RoundTripper
			log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", maskProxyURLHost(proxyURL))
		}
	}

	// Priority 4: Use RoundTripper from context (typically from RoundTripperFor)
	if httpClient.Transport == nil {
		if rt := sdkexecutor.RoundTripperFromContext(ctx); rt != nil {
			httpClient.Transport = rt
		} else if rt, ok := ctx.Value(util.ContextKeyRoundTripper).(http.RoundTripper); ok && rt != nil {
			httpClient.Transport = rt
		}
	}

	if httpClient.Transport == nil {
		transport := util.NewDefaultTransport(cfg != nil && cfg.PreferIPv4)
		httpClient.Transport = transport
		if sdkCfg := cfgToSDKCfg(cfg); sdkCfg != nil {
			util.ApplyTLSConfig(transport, sdkCfg)
		}
	}

	if proxyURL != "" {
		if manager := globalWarmManager.Load(); manager != nil && httpClient.Transport != nil {
			httpClient.Transport = &warmingRoundTripper{
				base:    httpClient.Transport,
				manager: manager,
			}
		}
	}
	if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil && httpClient.Transport != nil {
		httpClient.Transport = &upstreamTracingTransport{base: httpClient.Transport, ginCtx: ginCtx}
	}
	return httpClient
}

func recordRequestLogEgressRoute(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, proxyURL string) {
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if ginCtx == nil {
		return
	}

	proxyURL = strings.TrimSpace(proxyURL)
	route := requestLogEgressRoute{
		RouteKind: "direct",
	}
	if proxyURL != "" {
		route.RouteKind = "proxy"
		route.ProxyURLHost = maskProxyURLHost(proxyURL)
	}

	proxyID := ""
	fallbackURL := ""
	if auth != nil {
		proxyID = strings.TrimSpace(auth.ProxyID)
		fallbackURL = strings.TrimSpace(auth.ProxyURL)
	}
	route.ProxyID = proxyID

	switch {
	case proxyURL == "":
		route.ProxySource = "direct"
	case proxyID != "":
		route.ProxySource = "proxy_id"
	case fallbackURL != "" && strings.EqualFold(proxyURL, fallbackURL):
		route.ProxySource = "auth_proxy_url"
	case cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" && strings.EqualFold(proxyURL, cfg.ProxyURL):
		route.ProxySource = "global_proxy_url"
	default:
		route.ProxySource = "proxy_url"
	}

	if cfg != nil && proxyID != "" {
		for _, entry := range cfg.ProxyPool {
			if !entry.Enabled {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(entry.ID), proxyID) {
				route.ProxyName = strings.TrimSpace(entry.Name)
				break
			}
		}
	}

	ginCtx.Set(requestLogEgressRouteKey, route)
	diagnostics.SetEgress(ginCtx, requestLogEgressRouteMap(route))
}

func requestLogEgressRouteMap(route requestLogEgressRoute) map[string]any {
	result := map[string]any{}
	if route.RouteKind != "" {
		result["route_kind"] = route.RouteKind
	}
	if route.ProxySource != "" {
		result["proxy_source"] = route.ProxySource
	}
	if route.ProxyID != "" {
		result["proxy_id"] = route.ProxyID
	}
	if route.ProxyName != "" {
		result["proxy_name"] = route.ProxyName
	}
	if route.ProxyURLHost != "" {
		result["proxy_url_host"] = route.ProxyURLHost
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func maskProxyURLHost(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	return strings.ToLower(parsed.Scheme) + "://" + parsed.Host
}

// cfgToSDKCfg extracts the embedded SDKConfig from Config for TLS settings.
func cfgToSDKCfg(cfg *config.Config) *config.SDKConfig {
	if cfg == nil {
		return nil
	}
	return &cfg.SDKConfig
}

package executor

import (
	"context"
	"fmt"
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

type failingRoundTripper struct {
	err error
}

func (f failingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, f.err
}

// resolveAuthProxyURLStrict resolves the egress proxy for auth and fails closed
// when auth pins a proxy_id that is disabled or missing (with no explicit
// auth.ProxyURL fallback). Failing closed prevents OAuth refreshes and API
// calls from silently leaking out through the host IP or another account's proxy.
func resolveAuthProxyURLStrict(cfg *config.Config, auth *cliproxyauth.Auth) (string, error) {
	proxyID, fallbackURL := "", ""
	if auth != nil {
		proxyID = auth.ProxyID
		fallbackURL = auth.ProxyURL
	}
	if cfg == nil {
		if strings.TrimSpace(proxyID) != "" && strings.TrimSpace(fallbackURL) == "" {
			return "", fmt.Errorf("%w: proxy_id %q (config unavailable)", config.ErrBoundProxyUnavailable, config.NormalizeProxyID(proxyID))
		}
		return strings.TrimSpace(fallbackURL), nil
	}
	resolved, err := cfg.ResolveBoundProxyURL(proxyID, fallbackURL)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resolved), nil
}

// resolveAuthProxyURL returns the proxy a credential's own traffic goes out
// through, using the same precedence as newProxyAwareHTTPClient.
//
// OAuth token refresh must leave from this address rather than the process
// default. The two had drifted apart in production: requests went out through
// the credential's proxy-pool entry while refresh went direct, so upstream saw
// one account acting from two regions and rejected the refresh with
// `unsupported_country_region_territory` once the host region was unsupported.
// The access token then aged out and the account went unauthorized even though
// its request path was healthy.
//
// It returns "" when nothing is configured (or when a bound proxy is
// unavailable, in which case strict callers fail closed).
func resolveAuthProxyURL(cfg *config.Config, auth *cliproxyauth.Auth) string {
	resolved, err := resolveAuthProxyURLStrict(cfg, auth)
	if err != nil {
		return ""
	}
	return resolved
}

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyID when it resolves to an enabled proxy-pool entry
// 2. Use auth.ProxyURL if configured
// 3. Use cfg.ProxyURL if auth proxy is not configured
// 4. Use RoundTripper from context if neither are configured
//
// When a credential pins an unavailable proxy_id or an invalid proxy URL, the
// client fails closed instead of silently falling back to direct host egress.
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

	proxyURL, errProxy := resolveAuthProxyURLStrict(cfg, auth)
	if errProxy != nil {
		log.Warnf("bound proxy unavailable for auth request, failing closed: %v", errProxy)
		recordRequestLogEgressRoute(ctx, cfg, auth, "")
		httpClient.Transport = failingRoundTripper{err: errProxy}
		if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil {
			httpClient.Transport = &upstreamTracingTransport{base: httpClient.Transport, ginCtx: ginCtx}
		}
		return httpClient
	}
	recordRequestLogEgressRoute(ctx, cfg, auth, proxyURL)

	// viaProxy is set once the transport chosen really sends through proxyURL;
	// a proxy URL that cannot be used fails closed instead of falling through
	// to direct host egress, and the egress breaker only judges real proxy hops.
	viaProxy := false

	// A TLS fingerprint transport dials and tunnels on its own, because the
	// ClientHello has to be emitted by utls rather than by http.Transport. It
	// therefore replaces the transport selection below instead of wrapping it.
	if rt := tlsFingerprintRoundTripper(cfg, auth, proxyURL); rt != nil {
		httpClient.Transport = rt
		viaProxy = proxyURL != ""
	}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" && httpClient.Transport == nil {
		transport := cachedProxyTransport(proxyURL, cfgToSDKCfg(cfg))
		if transport != nil {
			httpClient.Transport = transport
			viaProxy = true
		} else {
			errInvalid := fmt.Errorf("invalid proxy configuration for %s: refusing direct fallback", maskProxyURLHost(proxyURL))
			log.Warnf("failed to setup proxy from URL %s, failing closed", maskProxyURLHost(proxyURL))
			httpClient.Transport = failingRoundTripper{err: errInvalid}
			if ginCtx, ok := ctx.Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil {
				httpClient.Transport = &upstreamTracingTransport{base: httpClient.Transport, ginCtx: ginCtx}
			}
			return httpClient
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
		httpClient.Transport = cachedDirectTransport(cfgToSDKCfg(cfg))
	}

	// Innermost wrapper, so that a request the breaker refuses still shows up
	// in the upstream timing and warm-up bookkeeping as an attempt that failed.
	if viaProxy {
		httpClient.Transport = egressBreaker.Transport(proxyURL, httpClient.Transport)
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

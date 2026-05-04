package executor

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

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

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := util.BuildProxyTransport(proxyURL)
		if transport != nil {
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 4: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value(util.ContextKeyRoundTripper).(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	return httpClient
}

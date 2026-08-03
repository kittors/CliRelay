package api

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

func corsMiddleware(cfgProvider func() *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c != nil && c.Request != nil && c.Request.URL != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/manage") {
				c.Next()
				return
			}
		}

		origin := ""
		if c != nil && c.Request != nil {
			origin = strings.TrimSpace(c.Request.Header.Get("Origin"))
		}
		if origin == "" {
			if c.Request.Method == http.MethodOptions {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
			c.Next()
			return
		}

		var cfg *config.Config
		if cfgProvider != nil {
			cfg = cfgProvider()
		}

		allowedOrigin := resolveAllowedCORSOrigin(c.Request, cfg)
		if allowedOrigin == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":  "origin not allowed",
				"origin": origin,
			})
			return
		}

		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Origin", allowedOrigin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin")
		c.Header("Access-Control-Expose-Headers", "X-CPA-VERSION, X-CPA-COMMIT, X-CPA-BUILD-DATE")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func resolveAllowedCORSOrigin(r *http.Request, cfg *config.Config) string {
	if r == nil {
		return ""
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return ""
	}

	if util.WebsocketOriginAllowed(&http.Request{
		Header: http.Header{"Origin": []string{origin}, "X-Forwarded-Host": r.Header.Values("X-Forwarded-Host")},
		Host:   r.Host,
	}) {
		return origin
	}

	if cfg == nil {
		return ""
	}
	for _, candidate := range cfg.CORSAllowOrigins {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		if strings.EqualFold(trimmed, origin) {
			return origin
		}
		// Explicit chrome-extension pattern only; not a general origin glob.
		if isChromeExtensionWildcard(trimmed) && isChromeExtensionOrigin(origin) {
			return origin
		}
	}
	return ""
}

// isChromeExtensionWildcard reports whether a configured allowlist entry is the
// controlled "any chrome extension" pattern. Only these exact forms match.
func isChromeExtensionWildcard(candidate string) bool {
	switch strings.ToLower(strings.TrimSpace(candidate)) {
	case "chrome-extension://*", "chrome-extension:*":
		return true
	default:
		return false
	}
}

// isChromeExtensionOrigin reports a real browser extension Origin
// (chrome-extension://<non-empty-id>), not a config pattern string.
func isChromeExtensionOrigin(origin string) bool {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed == nil {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "chrome-extension") {
		return false
	}
	host := strings.TrimSpace(parsed.Host)
	return host != "" && host != "*"
}

func versionHeaderMiddleware(configFilePath string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("x-cpa-version", buildinfo.Version)
		c.Header("x-cpa-build-date", buildinfo.BuildDate)
		currentUIVersion := buildinfo.FrontendVersion
		currentUICommit := buildinfo.FrontendCommit
		if meta, ok := managementasset.CurrentPanelMetadata(configFilePath); ok {
			if meta.Version != "" {
				currentUIVersion = meta.Version
			}
			if meta.Commit != "" {
				currentUICommit = meta.Commit
			}
		}
		c.Header("x-cpa-ui-version", currentUIVersion)
		c.Header("x-cpa-ui-commit", currentUICommit)
		c.Next()
	}
}

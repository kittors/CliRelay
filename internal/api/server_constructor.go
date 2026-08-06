package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// NewServer creates and initializes a new API server instance.
// It wires the HTTP shell around already-constructed runtime managers and
// returns a ready-to-start server façade.
func NewServer(cfg *config.Config, authManager *auth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	optionState := newServerOptionState(opts)
	configureServerMode(cfg)
	engine := newServerEngine(cfg, optionState)
	requestLogger, toggle := configureRequestLoggerMiddleware(engine, cfg, configFilePath, optionState)
	s := newServerRuntimeState(engine, cfg, authManager, accessManager, configFilePath, requestLogger, toggle)
	s.installDynamicMiddleware(configFilePath)
	s.applyInitialRuntimeConfig(cfg, authManager)
	s.configureManagementHandler(cfg, configFilePath, authManager, accessManager, optionState)
	s.setupRoutes()
	s.registerBuiltinModules(cfg, accessManager)
	s.applyRouterConfigurator(optionState, cfg)
	s.configureInitialManagementRoutes(cfg)
	s.configureInitialKeepAlive(optionState)
	s.server = buildHTTPServer(cfg, engine)
	return s
}

// wildcardProxyCIDRs trust every client's forwarding headers, which is
// indistinguishable from having no protection at all: any client could then
// choose its own throttle key and fill an arbitrary victim's bucket.
var wildcardProxyCIDRs = map[string]struct{}{"0.0.0.0/0": {}, "::/0": {}, "0.0.0.0": {}, "::": {}}

// sanitizeTrustedProxies trims the list and separates out the wildcard entries.
func sanitizeTrustedProxies(in []string) (out []string, dropped []string) {
	for _, proxy := range in {
		trimmed := strings.TrimSpace(proxy)
		if trimmed == "" {
			continue
		}
		if _, wildcard := wildcardProxyCIDRs[trimmed]; wildcard {
			dropped = append(dropped, trimmed)
			continue
		}
		out = append(out, trimmed)
	}
	return out, dropped
}

func configureTrustedProxies(engine *gin.Engine, trustedProxies []string) {
	if engine == nil {
		return
	}

	proxies, dropped := sanitizeTrustedProxies(trustedProxies)
	for _, d := range dropped {
		log.Errorf("trusted-proxies: refusing wildcard entry %q; it would trust every client's X-Forwarded-For and disable per-IP throttling. Entry dropped; list the actual proxy CIDR instead.", d)
	}

	if len(proxies) == 0 {
		if err := engine.SetTrustedProxies(nil); err != nil {
			log.Warnf("failed to disable trusted proxies: %v", err)
		}
		return
	}

	if err := engine.SetTrustedProxies(proxies); err != nil {
		log.Warnf("failed to configure trusted proxies %v: %v; forwarded client IP headers will be ignored", proxies, err)
		if fallbackErr := engine.SetTrustedProxies(nil); fallbackErr != nil {
			log.Warnf("failed to disable trusted proxies after configuration error: %v", fallbackErr)
		}
	}
}

// warnOnUntrustedProxyDeployment surfaces the single most common
// misconfiguration: remote management behind a reverse proxy with no
// trusted-proxies, where every user shares one throttle bucket.
//
// Warn and not fatal on purpose: refusing to start would turn an upgrade into an
// outage for every existing deployment that has this shape today, which is a
// larger incident than the degraded throttling it reports.
func warnOnUntrustedProxyDeployment(cfg *config.Config) {
	if cfg == nil || !cfg.RemoteManagement.AllowRemote {
		return
	}
	if proxies, _ := sanitizeTrustedProxies(cfg.TrustedProxies); len(proxies) > 0 {
		return
	}
	log.Warnf("remote management is enabled but trusted-proxies is empty. Behind a reverse proxy every client reports the proxy's address, so they all share one login-throttle bucket and per-IP hard bans are downgraded to soft rate limits. Set trusted-proxies to the proxy's CIDR (for example [\"127.0.0.1/32\"]) to restore per-client throttling.")
}

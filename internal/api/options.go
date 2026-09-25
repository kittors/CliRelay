package api

import (
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type serverOptionConfig struct {
	extraMiddleware            []gin.HandlerFunc
	engineConfigurator         func(*gin.Engine)
	routerConfigurator         func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory       func(*config.Config, string) logging.RequestLogger
	localPassword              string
	keepAliveEnabled           bool
	keepAliveTimeout           time.Duration
	keepAliveOnTimeout         func()
	postAuthHook               auth.PostAuthHook
	configMutatedCallback      func(*config.Config)
	modelConfigMutatedCallback func(tenantID string)
	configResyncCallback       func(*config.Config)
}

// ServerOption customises HTTP server construction.
type ServerOption func(*serverOptionConfig)

func defaultRequestLoggerFactory(cfg *config.Config, configPath string) logging.RequestLogger {
	configDir := filepath.Dir(configPath)
	if base := util.WritablePath(); base != "" {
		return logging.NewFileRequestLogger(cfg.RequestLog, filepath.Join(base, "logs"), configDir, cfg.ErrorLogsMaxFiles)
	}
	return logging.NewFileRequestLogger(cfg.RequestLog, "logs", configDir, cfg.ErrorLogsMaxFiles)
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.extraMiddleware = append(cfg.extraMiddleware, mw...)
	}
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.engineConfigurator = fn
	}
}

// WithRouterConfigurator appends a callback after default routes are registered.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.routerConfigurator = fn
	}
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.localPassword = password
	}
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return func(cfg *serverOptionConfig) {
		if timeout <= 0 || onTimeout == nil {
			return
		}
		cfg.keepAliveEnabled = true
		cfg.keepAliveTimeout = timeout
		cfg.keepAliveOnTimeout = onTimeout
	}
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.requestLoggerFactory = factory
	}
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthHook = hook
	}
}

func WithConfigMutatedCallback(fn func(*config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.configMutatedCallback = fn
	}
}

func WithModelConfigMutatedCallback(fn func(tenantID string)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.modelConfigMutatedCallback = fn
	}
}

// WithConfigResyncCallback registers the full configuration reload a cluster
// node falls back to when it cannot tell which settings other nodes changed.
// Unlike the config-mutated callback it must not re-register models.
func WithConfigResyncCallback(fn func(*config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.configResyncCallback = fn
	}
}

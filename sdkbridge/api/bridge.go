// Package apisdkbridge exposes SDK-facing API server bridges outside the sdk tree.
//
// The package lets sdk/api keep a stable public facade without importing
// internal server implementation paths directly.
package apisdkbridge

import (
	"time"

	"github.com/gin-gonic/gin"
	internalapisdkbridge "github.com/router-for-me/CLIProxyAPI/v6/internal/api/sdkbridge"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v6/sdk/logging"
)

type Server = internalapisdkbridge.Server

type ServerOption = internalapisdkbridge.ServerOption

// NewServer constructs the embedded API server bridge used by sdk/api.
func NewServer(cfg *sdkconfig.Config, authManager *coreauth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	return internalapisdkbridge.NewServer(cfg, authManager, accessManager, configFilePath, opts...)
}

func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return internalapisdkbridge.WithMiddleware(mw...)
}

func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return internalapisdkbridge.WithEngineConfigurator(fn)
}

func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *sdkconfig.Config)) ServerOption {
	return internalapisdkbridge.WithRouterConfigurator(fn)
}

func WithLocalManagementPassword(password string) ServerOption {
	return internalapisdkbridge.WithLocalManagementPassword(password)
}

func WithConfigMutatedCallback(fn func(*sdkconfig.Config)) ServerOption {
	return internalapisdkbridge.WithConfigMutatedCallback(fn)
}

func WithModelConfigMutatedCallback(fn func(tenantID string)) ServerOption {
	return internalapisdkbridge.WithModelConfigMutatedCallback(fn)
}

func WithConfigResyncCallback(fn func(*sdkconfig.Config)) ServerOption {
	return internalapisdkbridge.WithConfigResyncCallback(fn)
}

func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return internalapisdkbridge.WithKeepAliveEndpoint(timeout, onTimeout)
}

func WithSDKRequestLoggerFactory(factory func(*sdkconfig.Config, string) sdklogging.RequestLogger) ServerOption {
	return internalapisdkbridge.WithSDKRequestLoggerFactory(factory)
}

func WithPostAuthHook(hook coreauth.PostAuthHook) ServerOption {
	return internalapisdkbridge.WithPostAuthHook(hook)
}

type ManagementTokenRequester = internalapisdkbridge.ManagementTokenRequester

func NewManagementTokenRequester(cfg *sdkconfig.Config, manager *coreauth.Manager) ManagementTokenRequester {
	return internalapisdkbridge.NewManagementTokenRequester(cfg, manager)
}

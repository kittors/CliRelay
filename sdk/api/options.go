// Package api exposes the public embedding facade for CLIProxyAPI.
//
// The HTTP server implementation still lives under internal packages, but this
// package keeps the externally supported construction hooks and management
// helpers stable so SDK consumers do not need to import internal code directly.
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v6/sdk/logging"
	apisdkbridge "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/api"
)

// Server exposes the embedded HTTP server facade for SDK consumers.
//
// The concrete HTTP server implementation remains internal. This wrapper keeps
// the supported SDK lifecycle and wiring methods stable without re-exporting
// the internal server type identity.
type Server struct {
	inner *apisdkbridge.Server
}

// ServerOption customises HTTP server construction.
type ServerOption = apisdkbridge.ServerOption

// NewServer constructs a new embedded API server using SDK-facing types.
func NewServer(cfg *config.Config, authManager *coreauth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	return &Server{
		inner: apisdkbridge.NewServer(cfg, authManager, accessManager, configFilePath, opts...),
	}
}

// Start begins serving API traffic.
func (s *Server) Start() error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}
	return s.inner.Start()
}

// Stop gracefully shuts down the embedded HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Stop(ctx)
}

// UpdateClients refreshes runtime client state from the latest configuration snapshot.
func (s *Server) UpdateClients(cfg *config.Config) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.UpdateClients(cfg)
}

// AttachWebsocketRoute registers a websocket upgrade route on the embedded server.
func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.AttachWebsocketRoute(path, handler)
}

// SetWebsocketAuthChangeHandler registers a websocket auth toggle callback.
func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.SetWebsocketAuthChangeHandler(fn)
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption { return apisdkbridge.WithMiddleware(mw...) }

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return apisdkbridge.WithEngineConfigurator(fn)
}

// WithRouterConfigurator appends a callback after default routes are registered.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return apisdkbridge.WithRouterConfigurator(fn)
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return apisdkbridge.WithLocalManagementPassword(password)
}

// WithConfigMutatedCallback registers a callback invoked after management-side config mutations.
func WithConfigMutatedCallback(fn func(*config.Config)) ServerOption {
	return apisdkbridge.WithConfigMutatedCallback(fn)
}

// WithModelConfigMutatedCallback registers a callback invoked after management-side model catalog mutations.
func WithModelConfigMutatedCallback(fn func(tenantID string)) ServerOption {
	return apisdkbridge.WithModelConfigMutatedCallback(fn)
}

// WithConfigResyncCallback registers the full reload a cluster node falls back
// to when it cannot tell which settings other nodes changed.
func WithConfigResyncCallback(fn func(*config.Config)) ServerOption {
	return apisdkbridge.WithConfigResyncCallback(fn)
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return apisdkbridge.WithKeepAliveEndpoint(timeout, onTimeout)
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) sdklogging.RequestLogger) ServerOption {
	return apisdkbridge.WithSDKRequestLoggerFactory(factory)
}

// WithPostAuthHook registers a hook invoked after auth creation but before persistence.
func WithPostAuthHook(hook coreauth.PostAuthHook) ServerOption {
	return apisdkbridge.WithPostAuthHook(hook)
}

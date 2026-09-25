// Package apisdkbridge hosts the internal API bridge consumed by sdkbridge/api.
//
// It keeps server construction and management helpers behind one narrow import
// path so SDK-facing packages do not need to depend on the larger internal/api
// package directly.
package apisdkbridge

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	coreapi "github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	internalmanagement "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdklogging "github.com/router-for-me/CLIProxyAPI/v6/sdk/logging"
)

type Server struct {
	inner *coreapi.Server
}

type ServerOption = coreapi.ServerOption

func NewServer(cfg *sdkconfig.Config, authManager *coreauth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	return &Server{
		inner: coreapi.NewServer(cfg, authManager, accessManager, configFilePath, opts...),
	}
}

func (s *Server) Start() error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}
	return s.inner.Start()
}

func (s *Server) Stop(ctx context.Context) error {
	if s == nil || s.inner == nil {
		return nil
	}
	return s.inner.Stop(ctx)
}

func (s *Server) UpdateClients(cfg *sdkconfig.Config) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.UpdateClients(cfg)
}

func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.AttachWebsocketRoute(path, handler)
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil || s.inner == nil {
		return
	}
	s.inner.SetWebsocketAuthChangeHandler(fn)
}

func WithMiddleware(mw ...gin.HandlerFunc) ServerOption { return coreapi.WithMiddleware(mw...) }

func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return coreapi.WithEngineConfigurator(fn)
}

func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *sdkconfig.Config)) ServerOption {
	return coreapi.WithRouterConfigurator(fn)
}

func WithLocalManagementPassword(password string) ServerOption {
	return coreapi.WithLocalManagementPassword(password)
}

func WithConfigMutatedCallback(fn func(*sdkconfig.Config)) ServerOption {
	return coreapi.WithConfigMutatedCallback(fn)
}

func WithModelConfigMutatedCallback(fn func(tenantID string)) ServerOption {
	return coreapi.WithModelConfigMutatedCallback(fn)
}

func WithConfigResyncCallback(fn func(*sdkconfig.Config)) ServerOption {
	return coreapi.WithConfigResyncCallback(fn)
}

func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return coreapi.WithKeepAliveEndpoint(timeout, onTimeout)
}

func WithSDKRequestLoggerFactory(factory func(*sdkconfig.Config, string) sdklogging.RequestLogger) ServerOption {
	if factory == nil {
		return coreapi.WithRequestLoggerFactory(nil)
	}
	return coreapi.WithRequestLoggerFactory(func(cfg *sdkconfig.Config, configPath string) internallogging.RequestLogger {
		logger := factory(cfg, configPath)
		if logger == nil {
			return nil
		}
		return sdkRequestLoggerAdapter{inner: logger}
	})
}

func WithPostAuthHook(hook coreauth.PostAuthHook) ServerOption {
	return coreapi.WithPostAuthHook(hook)
}

type ManagementTokenRequester interface {
	RequestAnthropicToken(*gin.Context)
	RequestGeminiCLIToken(*gin.Context)
	RequestCodexToken(*gin.Context)
	RequestAntigravityToken(*gin.Context)
	RequestQwenToken(*gin.Context)
	RequestKimiToken(*gin.Context)
	RequestIFlowToken(*gin.Context)
	RequestIFlowCookieToken(*gin.Context)
	GetAuthStatus(*gin.Context)
	PostOAuthCallback(*gin.Context)
}

func NewManagementTokenRequester(cfg *sdkconfig.Config, manager *coreauth.Manager) ManagementTokenRequester {
	return internalmanagement.NewHandlerWithoutConfigFilePath(cfg, manager)
}

type sdkRequestLoggerAdapter struct {
	inner sdklogging.RequestLogger
}

func (a sdkRequestLoggerAdapter) LogRequest(url, method string, requestHeaders map[string][]string, body []byte, statusCode int, responseHeaders map[string][]string, response, apiRequest, apiResponse []byte, apiResponseErrors []*interfaces.ErrorMessage, requestID string, requestTimestamp, apiResponseTimestamp time.Time) error {
	return a.inner.LogRequest(url, method, requestHeaders, body, statusCode, responseHeaders, response, apiRequest, apiResponse, convertErrorMessages(apiResponseErrors), requestID, requestTimestamp, apiResponseTimestamp)
}

func (a sdkRequestLoggerAdapter) LogStreamingRequest(url, method string, headers map[string][]string, body []byte, requestID string) (internallogging.StreamingLogWriter, error) {
	writer, err := a.inner.LogStreamingRequest(url, method, headers, body, requestID)
	if err != nil {
		return nil, err
	}
	if writer == nil {
		return nil, nil
	}
	return sdkStreamingLogWriterAdapter{inner: writer}, nil
}

func (a sdkRequestLoggerAdapter) IsEnabled() bool {
	return a.inner.IsEnabled()
}

type sdkStreamingLogWriterAdapter struct {
	inner sdklogging.StreamingLogWriter
}

func (a sdkStreamingLogWriterAdapter) WriteChunkAsync(chunk []byte) {
	a.inner.WriteChunkAsync(chunk)
}

func (a sdkStreamingLogWriterAdapter) NoteDroppedChunks(n int) {
	a.inner.NoteDroppedChunks(n)
}

func (a sdkStreamingLogWriterAdapter) WriteStatus(status int, headers map[string][]string) error {
	return a.inner.WriteStatus(status, headers)
}

func (a sdkStreamingLogWriterAdapter) WriteAPIRequest(apiRequest []byte) error {
	return a.inner.WriteAPIRequest(apiRequest)
}

func (a sdkStreamingLogWriterAdapter) WriteAPIResponse(apiResponse []byte) error {
	return a.inner.WriteAPIResponse(apiResponse)
}

func (a sdkStreamingLogWriterAdapter) SetFirstChunkTimestamp(timestamp time.Time) {
	a.inner.SetFirstChunkTimestamp(timestamp)
}

func (a sdkStreamingLogWriterAdapter) Close() error {
	return a.inner.Close()
}

func convertErrorMessages(src []*interfaces.ErrorMessage) []*sdklogging.ErrorMessage {
	if len(src) == 0 {
		return nil
	}
	out := make([]*sdklogging.ErrorMessage, 0, len(src))
	for _, item := range src {
		if item == nil {
			continue
		}
		out = append(out, &sdklogging.ErrorMessage{
			StatusCode: item.StatusCode,
			Error:      item.Error,
			Addon:      item.Addon,
		})
	}
	return out
}

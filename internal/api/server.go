// Package api provides the HTTP API server implementation for the CLI Proxy API.
// It includes the main server struct, routing setup, middleware for CORS and authentication,
// and integration with various AI API handlers (OpenAI, Claude, Gemini).
// The server supports hot-reloading of clients and configuration.
package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/access"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/modules"
	ampmodule "github.com/router-for-me/CLIProxyAPI/v6/internal/api/modules/amp"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v3"
)

const (
	oauthCallbackSuccessHTML  = `<html><head><meta charset="utf-8"><title>Authentication successful</title><script>setTimeout(function(){window.close();},5000);</script></head><body><h1>Authentication successful!</h1><p>You can close this window.</p><p>This window will close automatically in 5 seconds.</p></body></html>`
	mainAPIServerWriteTimeout = 10 * time.Minute
)

type serverOptionConfig struct {
	extraMiddleware      []gin.HandlerFunc
	engineConfigurator   func(*gin.Engine)
	routerConfigurator   func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory func(*config.Config, string) logging.RequestLogger
	localPassword        string
	keepAliveEnabled     bool
	keepAliveTimeout     time.Duration
	keepAliveOnTimeout   func()
	postAuthHook         auth.PostAuthHook
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

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	// handlers contains the API handlers for processing requests.
	handlers *handlers.BaseAPIHandler

	// cfg holds the current server configuration.
	cfg *config.Config

	// oldConfigYaml stores a YAML snapshot of the previous configuration for change detection.
	// This prevents issues when the config object is modified in place by Management API.
	oldConfigYaml []byte

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// requestLogger is the request logger instance for dynamic configuration updates.
	requestLogger logging.RequestLogger
	loggerToggle  func(bool)

	// configFilePath is the absolute path to the YAML config file for persistence.
	configFilePath string

	// currentPath is the absolute path to the current working directory.
	currentPath string

	// wsRoutes tracks registered websocket upgrade paths.
	wsRouteMu     sync.Mutex
	wsRoutes      map[string]struct{}
	wsAuthChanged func(bool, bool)
	wsAuthEnabled atomic.Bool

	// management handler
	mgmt *managementHandlers.Handler

	// ampModule is the Amp routing module for model mapping hot-reload
	ampModule *ampmodule.AmpModule

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled atomic.Bool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()
	keepAliveHeartbeat chan struct{}
	keepAliveStop      chan struct{}

	publicLookupRateMu          sync.Mutex
	publicLookupRate            map[string]publicLookupRateLimitEntry
	publicLookupRateLastCleanup time.Time
}

// NewServer creates and initializes a new API server instance.
// It sets up the Gin engine, middleware, routes, and handlers.
//
// Parameters:
//   - cfg: The server configuration
//   - authManager: core runtime auth manager
//   - accessManager: request authentication manager
//
// Returns:
//   - *Server: A new server instance
func NewServer(cfg *config.Config, authManager *auth.Manager, accessManager *sdkaccess.Manager, configFilePath string, opts ...ServerOption) *Server {
	optionState := &serverOptionConfig{
		requestLoggerFactory: defaultRequestLoggerFactory,
	}
	for i := range opts {
		opts[i](optionState)
	}
	// Set gin mode
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create gin engine
	engine := gin.New()
	if err := engine.SetTrustedProxies(nil); err != nil {
		log.Warnf("failed to disable trusted proxies: %v", err)
	}
	if optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	// Add middleware
	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
	for _, mw := range optionState.extraMiddleware {
		engine.Use(mw)
	}

	// Add request logging middleware (positioned after recovery, before auth)
	// Resolve logs directory relative to the configuration file directory.
	var requestLogger logging.RequestLogger
	var toggle func(bool)
	if !cfg.CommercialMode {
		if optionState.requestLoggerFactory != nil {
			requestLogger = optionState.requestLoggerFactory(cfg, configFilePath)
		}
		if requestLogger != nil {
			engine.Use(middleware.RequestLoggingMiddleware(requestLogger))
			if setter, ok := requestLogger.(interface{ SetEnabled(bool) }); ok {
				toggle = setter.SetEnabled
			}
		}
	}

	var s *Server
	engine.Use(corsMiddleware(func() *config.Config {
		if s != nil && s.cfg != nil {
			return s.cfg
		}
		return cfg
	}))
	engine.Use(versionHeaderMiddleware(configFilePath))
	wd, err := os.Getwd()
	if err != nil {
		wd = configFilePath
	}

	envAdminPassword, envAdminPasswordSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envAdminPassword = strings.TrimSpace(envAdminPassword)
	envManagementSecret := envAdminPasswordSet && envAdminPassword != ""

	// Create server instance
	s = &Server{
		engine:              engine,
		handlers:            handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager),
		cfg:                 cfg,
		accessManager:       accessManager,
		requestLogger:       requestLogger,
		loggerToggle:        toggle,
		configFilePath:      configFilePath,
		currentPath:         wd,
		envManagementSecret: envManagementSecret,
		wsRoutes:            make(map[string]struct{}),
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	// Save initial YAML snapshot
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
	s.applyAccessConfig(nil, cfg)
	if authManager != nil {
		authManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second)
	}
	managementasset.SetCurrentConfig(cfg)
	auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	// Initialize management handler
	s.mgmt = managementHandlers.NewHandler(cfg, configFilePath, authManager)
	s.mgmt.SetAccessManager(accessManager)
	if optionState.localPassword != "" {
		s.mgmt.SetLocalPassword(optionState.localPassword)
	}
	logDir := logging.ResolveLogDirectory(cfg)
	s.mgmt.SetLogDirectory(logDir)
	if optionState.postAuthHook != nil {
		s.mgmt.SetPostAuthHook(optionState.postAuthHook)
	}
	s.localPassword = optionState.localPassword

	// Setup routes
	s.setupRoutes()

	// Register Amp module using V2 interface with Context
	s.ampModule = ampmodule.NewLegacy(accessManager, AuthMiddleware(accessManager))
	ctx := modules.Context{
		Engine:         engine,
		BaseHandler:    s.handlers,
		Config:         cfg,
		AuthMiddleware: AuthMiddleware(accessManager),
	}
	if err := modules.RegisterModule(ctx, s.ampModule); err != nil {
		log.Errorf("Failed to register Amp module: %v", err)
	}

	// Apply additional router configurators from options
	if optionState.routerConfigurator != nil {
		optionState.routerConfigurator(engine, s.handlers, cfg)
	}

	// Register management routes when configuration or environment secrets are available,
	// or when a local management password is provided (e.g. TUI mode).
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
	s.managementRoutesEnabled.Store(hasManagementSecret)
	if hasManagementSecret {
		s.registerManagementRoutes()
	}

	if optionState.keepAliveEnabled {
		s.enableKeepAlive(optionState.keepAliveTimeout, optionState.keepAliveOnTimeout)
	}

	// Create HTTP server
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      mainAPIServerWriteTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	return s
}

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	s.engine.GET("/management.html", s.serveManagementControlPanel)
	s.engine.GET("/manage", s.serveManagementControlPanel)
	s.engine.GET("/manage/*filepath", s.serveManagementControlPanel)
	openaiHandlers := openai.NewOpenAIAPIHandler(s.handlers)
	geminiHandlers := gemini.NewGeminiAPIHandler(s.handlers)
	geminiCLIHandlers := gemini.NewGeminiCLIAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewClaudeCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewOpenAIResponsesAPIHandler(s.handlers)
	openaiImagesHandlers := openai.NewOpenAIImagesAPIHandler(s.handlers)

	registerV1Routes := func(group *gin.RouterGroup) {
		group.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		group.POST("/chat/completions", openaiHandlers.ChatCompletions)
		group.POST("/completions", openaiHandlers.Completions)
		group.POST("/images/generations", openaiImagesHandlers.Generations)
		group.POST("/images/edits", openaiImagesHandlers.Edits)
		group.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		group.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		group.GET("/responses", func(c *gin.Context) {
			clearServerWriteDeadline(c)
			openaiResponsesHandlers.ResponsesWebsocket(c)
		})
		group.POST("/responses", openaiResponsesHandlers.Responses)
		group.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}
	registerV1BetaRoutes := func(group *gin.RouterGroup) {
		group.GET("/models", geminiHandlers.GeminiModels)
		group.POST("/models/*action", geminiHandlers.GeminiHandler)
		group.GET("/models/*action", geminiHandlers.GeminiGetHandler)
	}
	resolveRoute := func(rawGroup string) (*internalrouting.PathRouteContext, bool) {
		return resolvePathRouteContext(s.cfg, s.handlers.AuthManager, rawGroup)
	}

	// OpenAI compatible API routes
	v1 := s.engine.Group("/v1")
	v1.Use(AuthMiddleware(s.accessManager))
	v1.Use(channelGroupAuthorizationMiddleware())
	v1.Use(middleware.QuotaMiddleware())
	v1.Use(ModelRestrictionMiddleware())
	v1.Use(SystemPromptMiddleware())
	registerV1Routes(v1)

	groupedV1 := s.engine.Group("/:group/v1")
	groupedV1.Use(groupRoutingMiddleware(resolveRoute))
	groupedV1.Use(AuthMiddleware(s.accessManager))
	groupedV1.Use(channelGroupAuthorizationMiddleware())
	groupedV1.Use(middleware.QuotaMiddleware())
	groupedV1.Use(ModelRestrictionMiddleware())
	groupedV1.Use(SystemPromptMiddleware())
	registerV1Routes(groupedV1)

	// Gemini compatible API routes
	v1beta := s.engine.Group("/v1beta")
	v1beta.Use(AuthMiddleware(s.accessManager))
	v1beta.Use(channelGroupAuthorizationMiddleware())
	v1beta.Use(middleware.QuotaMiddleware())
	v1beta.Use(ModelRestrictionMiddleware())
	registerV1BetaRoutes(v1beta)

	groupedV1Beta := s.engine.Group("/:group/v1beta")
	groupedV1Beta.Use(groupRoutingMiddleware(resolveRoute))
	groupedV1Beta.Use(AuthMiddleware(s.accessManager))
	groupedV1Beta.Use(channelGroupAuthorizationMiddleware())
	groupedV1Beta.Use(middleware.QuotaMiddleware())
	groupedV1Beta.Use(ModelRestrictionMiddleware())
	registerV1BetaRoutes(groupedV1Beta)

	s.engine.NoRoute(func(c *gin.Context) {
		if _, rewritten := c.Get("cliproxy.grouped_path_rewrite"); rewritten {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		rawGroupPath, apiPath, ok := splitGroupedAPIPath(c.Request.URL.Path)
		if !ok {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		route, ok := resolveRoute(rawGroupPath)
		if !ok || route == nil {
			abortChannelGroupRouteNotFound(c)
			return
		}
		attachPathRouteContext(c, route)
		c.Set("cliproxy.grouped_path_rewrite", true)
		c.Request.URL.Path = apiPath
		if c.Request.URL.RawQuery != "" {
			c.Request.RequestURI = apiPath + "?" + c.Request.URL.RawQuery
		} else {
			c.Request.RequestURI = apiPath
		}
		// Gin's NoRoute preloads 404 before our rewrite. Reset to 200 so the
		// rewritten handler can emit a normal success status when it does not
		// explicitly call WriteHeader itself.
		c.Status(http.StatusOK)
		s.engine.HandleContext(c)
		// HandleContext resets c.handlers to the rewritten route chain but
		// restores the old index afterwards. Abort the outer NoRoute chain so
		// Gin cannot continue into the rewritten handlers a second time.
		c.Abort()
	})

	// Root endpoint
	s.engine.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "CLI Proxy API Server",
			"endpoints": []string{
				"POST /v1/chat/completions",
				"POST /v1/completions",
				"POST /v1/images/generations",
				"GET /v1/models",
			},
		})
	})
	s.engine.POST("/v1internal:method", geminiCLIHandlers.CLIHandler)

	// OAuth callback endpoints (reuse main server port)
	// These endpoints receive provider redirects and persist
	// the short-lived code/state for the waiting goroutine.
	s.engine.GET("/anthropic/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "anthropic", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/codex/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "codex", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/google/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "gemini", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/iflow/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "iflow", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	s.engine.GET("/antigravity/callback", func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errStr := c.Query("error")
		if errStr == "" {
			errStr = c.Query("error_description")
		}
		if state != "" {
			_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(s.cfg.AuthDir, "antigravity", state, code, errStr)
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	})

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// AttachWebsocketRoute registers a websocket upgrade handler on the primary Gin engine.
// The handler is served as-is without additional middleware beyond the standard stack already configured.
func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.engine == nil || handler == nil {
		return
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/v1/ws"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	s.wsRouteMu.Lock()
	if _, exists := s.wsRoutes[trimmed]; exists {
		s.wsRouteMu.Unlock()
		return
	}
	s.wsRoutes[trimmed] = struct{}{}
	s.wsRouteMu.Unlock()

	authMiddleware := AuthMiddleware(s.accessManager)
	conditionalAuth := func(c *gin.Context) {
		if !s.wsAuthEnabled.Load() {
			c.Next()
			return
		}
		authMiddleware(c)
	}
	finalHandler := func(c *gin.Context) {
		clearServerWriteDeadline(c)
		handler.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}

	s.engine.GET(trimmed, conditionalAuth, finalHandler)
}

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	mgmt := s.engine.Group("/v0/management")
	mgmt.Use(s.managementAvailabilityMiddleware(), s.mgmt.Middleware(), bodyutil.LimitBodyMiddleware(bodyutil.ManagementBodyLimit))
	{
		mgmt.GET("/dashboard-summary", s.mgmt.GetDashboardSummary)
		mgmt.GET("/system-stats", s.mgmt.GetSystemStats)
		mgmt.GET("/system-stats/ws", func(c *gin.Context) {
			clearServerWriteDeadline(c)
			s.mgmt.SystemStatsWebSocket(c)
		})
		mgmt.GET("/models", s.mgmt.GetModels)
		mgmt.GET("/model-configs", s.mgmt.GetModelConfigs)
		mgmt.POST("/model-configs", s.mgmt.PostModelConfig)
		mgmt.PUT("/model-configs/*id", s.mgmt.PutModelConfig)
		mgmt.DELETE("/model-configs/*id", s.mgmt.DeleteModelConfig)
		mgmt.GET("/model-owner-presets", s.mgmt.GetModelOwnerPresets)
		mgmt.PUT("/model-owner-presets", s.mgmt.PutModelOwnerPresets)
		mgmt.GET("/model-openrouter-sync", s.mgmt.GetOpenRouterModelSync)
		mgmt.PUT("/model-openrouter-sync", s.mgmt.PutOpenRouterModelSync)
		mgmt.POST("/model-openrouter-sync/run", s.mgmt.PostOpenRouterModelSyncRun)
		mgmt.GET("/channel-groups", s.mgmt.GetChannelGroups)
		mgmt.GET("/routing-config", s.mgmt.GetRoutingConfig)
		mgmt.PUT("/routing-config", s.mgmt.PutRoutingConfig)
		mgmt.GET("/identity-fingerprint", s.mgmt.GetIdentityFingerprint)
		mgmt.PUT("/identity-fingerprint", s.mgmt.PutIdentityFingerprint)
		mgmt.GET("/model-pricing", s.mgmt.GetModelPricing)
		mgmt.PUT("/model-pricing", s.mgmt.PutModelPricing)
		mgmt.GET("/usage", s.mgmt.GetUsageStatistics)
		mgmt.GET("/usage/export", s.mgmt.ExportUsageStatistics)
		mgmt.POST("/usage/import", s.mgmt.ImportUsageStatistics)
		mgmt.GET("/usage/logs", s.mgmt.GetUsageLogs)
		mgmt.GET("/usage/logs/:id/content", s.mgmt.GetLogContent)
		mgmt.GET("/usage/auth-file-group-trend", s.mgmt.GetAuthFileGroupTrend)
		mgmt.GET("/usage/auth-file-trend", s.mgmt.GetAuthFileTrend)
		mgmt.POST("/usage/auth-file-quota-snapshot", s.mgmt.PostAuthFileQuotaSnapshot)
		mgmt.GET("/usage/chart-data", s.mgmt.GetUsageChartData)
		mgmt.GET("/usage/entity-stats", s.mgmt.GetEntityUsageStats)
		mgmt.GET("/config", s.mgmt.GetConfig)
		mgmt.GET("/config.yaml", s.mgmt.GetConfigYAML)
		mgmt.PUT("/config.yaml", s.mgmt.PutConfigYAML)
		mgmt.GET("/latest-version", s.mgmt.GetLatestVersion)
		mgmt.GET("/update/check", s.mgmt.CheckUpdate)
		mgmt.GET("/update/current", s.mgmt.GetCurrentUpdateState)
		mgmt.GET("/update/progress", s.mgmt.GetUpdateProgress)
		mgmt.POST("/update/apply", s.mgmt.ApplyUpdate)
		mgmt.GET("/auto-update/enabled", s.mgmt.GetAutoUpdateEnabled)
		mgmt.PUT("/auto-update/enabled", s.mgmt.PutAutoUpdateEnabled)
		mgmt.PATCH("/auto-update/enabled", s.mgmt.PutAutoUpdateEnabled)
		mgmt.GET("/auto-update/channel", s.mgmt.GetAutoUpdateChannel)
		mgmt.PUT("/auto-update/channel", s.mgmt.PutAutoUpdateChannel)
		mgmt.PATCH("/auto-update/channel", s.mgmt.PutAutoUpdateChannel)

		mgmt.GET("/debug", s.mgmt.GetDebug)
		mgmt.PUT("/debug", s.mgmt.PutDebug)
		mgmt.PATCH("/debug", s.mgmt.PutDebug)

		mgmt.GET("/logging-to-file", s.mgmt.GetLoggingToFile)
		mgmt.PUT("/logging-to-file", s.mgmt.PutLoggingToFile)
		mgmt.PATCH("/logging-to-file", s.mgmt.PutLoggingToFile)

		mgmt.GET("/logs-max-total-size-mb", s.mgmt.GetLogsMaxTotalSizeMB)
		mgmt.PUT("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)
		mgmt.PATCH("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)

		mgmt.GET("/error-logs-max-files", s.mgmt.GetErrorLogsMaxFiles)
		mgmt.PUT("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)
		mgmt.PATCH("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)

		mgmt.GET("/usage-statistics-enabled", s.mgmt.GetUsageStatisticsEnabled)
		mgmt.PUT("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.PATCH("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)

		mgmt.GET("/proxy-url", s.mgmt.GetProxyURL)
		mgmt.PUT("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.PATCH("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.DELETE("/proxy-url", s.mgmt.DeleteProxyURL)
		mgmt.GET("/proxy-pool", s.mgmt.GetProxyPool)
		mgmt.PUT("/proxy-pool", s.mgmt.PutProxyPool)
		mgmt.POST("/proxy-pool/check", s.mgmt.PostProxyPoolCheck)

		mgmt.POST("/api-call", s.mgmt.APICall)

		mgmt.GET("/quota-exceeded/switch-project", s.mgmt.GetSwitchProject)
		mgmt.PUT("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)
		mgmt.PATCH("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)

		mgmt.GET("/quota-exceeded/switch-preview-model", s.mgmt.GetSwitchPreviewModel)
		mgmt.PUT("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.PATCH("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.POST("/quota/reconcile", s.mgmt.PostQuotaReconcile)

		mgmt.GET("/api-keys", s.mgmt.GetAPIKeys)
		mgmt.PUT("/api-keys", s.mgmt.PutAPIKeys)
		mgmt.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
		mgmt.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)

		mgmt.GET("/api-key-entries", s.mgmt.GetAPIKeyEntries)
		mgmt.PUT("/api-key-entries", s.mgmt.PutAPIKeyEntries)
		mgmt.PATCH("/api-key-entries", s.mgmt.PatchAPIKeyEntry)
		mgmt.DELETE("/api-key-entries", s.mgmt.DeleteAPIKeyEntry)

		mgmt.GET("/gemini-api-key", s.mgmt.GetGeminiKeys)
		mgmt.PUT("/gemini-api-key", s.mgmt.PutGeminiKeys)
		mgmt.PATCH("/gemini-api-key", s.mgmt.PatchGeminiKey)
		mgmt.DELETE("/gemini-api-key", s.mgmt.DeleteGeminiKey)

		mgmt.GET("/logs", s.mgmt.GetLogs)
		mgmt.DELETE("/logs", s.mgmt.DeleteLogs)
		mgmt.GET("/request-error-logs", s.mgmt.GetRequestErrorLogs)
		mgmt.GET("/request-error-logs/:name", s.mgmt.DownloadRequestErrorLog)
		mgmt.GET("/request-log-by-id/:id", s.mgmt.GetRequestLogByID)
		mgmt.GET("/request-log", s.mgmt.GetRequestLog)
		mgmt.PUT("/request-log", s.mgmt.PutRequestLog)
		mgmt.PATCH("/request-log", s.mgmt.PutRequestLog)
		mgmt.GET("/ws-auth", s.mgmt.GetWebsocketAuth)
		mgmt.PUT("/ws-auth", s.mgmt.PutWebsocketAuth)
		mgmt.PATCH("/ws-auth", s.mgmt.PutWebsocketAuth)

		mgmt.GET("/ampcode", s.mgmt.GetAmpCode)
		mgmt.GET("/ampcode/upstream-url", s.mgmt.GetAmpUpstreamURL)
		mgmt.PUT("/ampcode/upstream-url", s.mgmt.PutAmpUpstreamURL)
		mgmt.PATCH("/ampcode/upstream-url", s.mgmt.PutAmpUpstreamURL)
		mgmt.DELETE("/ampcode/upstream-url", s.mgmt.DeleteAmpUpstreamURL)
		mgmt.GET("/ampcode/upstream-api-key", s.mgmt.GetAmpUpstreamAPIKey)
		mgmt.PUT("/ampcode/upstream-api-key", s.mgmt.PutAmpUpstreamAPIKey)
		mgmt.PATCH("/ampcode/upstream-api-key", s.mgmt.PutAmpUpstreamAPIKey)
		mgmt.DELETE("/ampcode/upstream-api-key", s.mgmt.DeleteAmpUpstreamAPIKey)
		mgmt.GET("/ampcode/restrict-management-to-localhost", s.mgmt.GetAmpRestrictManagementToLocalhost)
		mgmt.PUT("/ampcode/restrict-management-to-localhost", s.mgmt.PutAmpRestrictManagementToLocalhost)
		mgmt.PATCH("/ampcode/restrict-management-to-localhost", s.mgmt.PutAmpRestrictManagementToLocalhost)
		mgmt.GET("/ampcode/model-mappings", s.mgmt.GetAmpModelMappings)
		mgmt.PUT("/ampcode/model-mappings", s.mgmt.PutAmpModelMappings)
		mgmt.PATCH("/ampcode/model-mappings", s.mgmt.PatchAmpModelMappings)
		mgmt.DELETE("/ampcode/model-mappings", s.mgmt.DeleteAmpModelMappings)
		mgmt.GET("/ampcode/force-model-mappings", s.mgmt.GetAmpForceModelMappings)
		mgmt.PUT("/ampcode/force-model-mappings", s.mgmt.PutAmpForceModelMappings)
		mgmt.PATCH("/ampcode/force-model-mappings", s.mgmt.PutAmpForceModelMappings)
		mgmt.GET("/ampcode/upstream-api-keys", s.mgmt.GetAmpUpstreamAPIKeys)
		mgmt.PUT("/ampcode/upstream-api-keys", s.mgmt.PutAmpUpstreamAPIKeys)
		mgmt.PATCH("/ampcode/upstream-api-keys", s.mgmt.PatchAmpUpstreamAPIKeys)
		mgmt.DELETE("/ampcode/upstream-api-keys", s.mgmt.DeleteAmpUpstreamAPIKeys)

		mgmt.GET("/request-retry", s.mgmt.GetRequestRetry)
		mgmt.PUT("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.PATCH("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.GET("/max-retry-interval", s.mgmt.GetMaxRetryInterval)
		mgmt.PUT("/max-retry-interval", s.mgmt.PutMaxRetryInterval)
		mgmt.PATCH("/max-retry-interval", s.mgmt.PutMaxRetryInterval)

		mgmt.GET("/force-model-prefix", s.mgmt.GetForceModelPrefix)
		mgmt.PUT("/force-model-prefix", s.mgmt.PutForceModelPrefix)
		mgmt.PATCH("/force-model-prefix", s.mgmt.PutForceModelPrefix)

		mgmt.GET("/routing/strategy", s.mgmt.GetRoutingStrategy)
		mgmt.PUT("/routing/strategy", s.mgmt.PutRoutingStrategy)
		mgmt.PATCH("/routing/strategy", s.mgmt.PutRoutingStrategy)

		mgmt.GET("/claude-api-key", s.mgmt.GetClaudeKeys)
		mgmt.PUT("/claude-api-key", s.mgmt.PutClaudeKeys)
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/bedrock-api-key", s.mgmt.GetBedrockKeys)
		mgmt.PUT("/bedrock-api-key", s.mgmt.PutBedrockKeys)
		mgmt.PATCH("/bedrock-api-key", s.mgmt.PatchBedrockKey)
		mgmt.DELETE("/bedrock-api-key", s.mgmt.DeleteBedrockKey)

		mgmt.GET("/opencode-go-api-key", s.mgmt.GetOpenCodeGoKeys)
		mgmt.PUT("/opencode-go-api-key", s.mgmt.PutOpenCodeGoKeys)
		mgmt.PATCH("/opencode-go-api-key", s.mgmt.PatchOpenCodeGoKey)
		mgmt.DELETE("/opencode-go-api-key", s.mgmt.DeleteOpenCodeGoKey)

		mgmt.GET("/codex-api-key", s.mgmt.GetCodexKeys)
		mgmt.PUT("/codex-api-key", s.mgmt.PutCodexKeys)
		mgmt.PATCH("/codex-api-key", s.mgmt.PatchCodexKey)
		mgmt.DELETE("/codex-api-key", s.mgmt.DeleteCodexKey)

		mgmt.GET("/openai-compatibility", s.mgmt.GetOpenAICompat)
		mgmt.PUT("/openai-compatibility", s.mgmt.PutOpenAICompat)
		mgmt.PATCH("/openai-compatibility", s.mgmt.PatchOpenAICompat)
		mgmt.DELETE("/openai-compatibility", s.mgmt.DeleteOpenAICompat)

		mgmt.GET("/vertex-api-key", s.mgmt.GetVertexCompatKeys)
		mgmt.PUT("/vertex-api-key", s.mgmt.PutVertexCompatKeys)
		mgmt.PATCH("/vertex-api-key", s.mgmt.PatchVertexCompatKey)
		mgmt.DELETE("/vertex-api-key", s.mgmt.DeleteVertexCompatKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)

		mgmt.GET("/auth-files", s.mgmt.ListAuthFiles)
		mgmt.GET("/auth-files/models", s.mgmt.GetAuthFileModels)
		mgmt.GET("/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)
		mgmt.GET("/image-generation/channels", s.mgmt.ListImageGenerationChannels)
		mgmt.POST("/image-generation/test", s.mgmt.PostImageGenerationTest)
		mgmt.GET("/image-generation/test/:task_id", s.mgmt.GetImageGenerationTestTask)
		mgmt.GET("/auth-files/download", s.mgmt.DownloadAuthFile)
		mgmt.POST("/auth-files", s.mgmt.UploadAuthFile)
		mgmt.DELETE("/auth-files", s.mgmt.DeleteAuthFile)
		mgmt.PATCH("/auth-files/status", s.mgmt.PatchAuthFileStatus)
		mgmt.PATCH("/auth-files/fields", s.mgmt.PatchAuthFileFields)
		mgmt.POST("/vertex/import", s.mgmt.ImportVertexCredential)

		mgmt.GET("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.GET("/codex-auth-url", s.mgmt.RequestCodexToken)
		mgmt.GET("/gemini-cli-auth-url", s.mgmt.RequestGeminiCLIToken)
		mgmt.GET("/antigravity-auth-url", s.mgmt.RequestAntigravityToken)
		mgmt.GET("/qwen-auth-url", s.mgmt.RequestQwenToken)
		mgmt.GET("/kimi-auth-url", s.mgmt.RequestKimiToken)
		mgmt.GET("/iflow-auth-url", s.mgmt.RequestIFlowToken)
		mgmt.POST("/iflow-auth-url", s.mgmt.RequestIFlowCookieToken)
		mgmt.POST("/oauth-callback", s.mgmt.PostOAuthCallback)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
	}

	// Public endpoints - no management key required
	pub := s.engine.Group("/v0/management/public")
	pub.Use(s.managementAvailabilityMiddleware(), publicLookupNoStoreMiddleware(), s.publicLookupRateLimitMiddleware())
	{
		pub.GET("/usage", s.mgmt.GetPublicUsageByAPIKey)
		pub.POST("/usage", s.mgmt.GetPublicUsageByAPIKey)
		pub.GET("/usage/logs", s.mgmt.GetPublicUsageLogs)
		pub.POST("/usage/logs", s.mgmt.GetPublicUsageLogs)
		pub.GET("/usage/logs/:id/content", s.mgmt.GetPublicLogContent)
		pub.POST("/usage/logs/:id/content", s.mgmt.GetPublicLogContent)
		pub.GET("/usage/chart-data", s.mgmt.GetPublicUsageChartData)
		pub.POST("/usage/chart-data", s.mgmt.GetPublicUsageChartData)
	}
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.managementRoutesEnabled.Load() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// Resolve the directory containing the SPA assets (manage.html + assets/).
	panelDir := s.resolvePanelDir()
	if panelDir == "" {
		// Fallback to legacy single-file management.html behavior.
		filePath := managementasset.FilePath(s.configFilePath)
		if strings.TrimSpace(filePath) == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if _, err := os.Stat(filePath); err != nil {
			if os.IsNotExist(err) {
				reqCtx := context.Background()
				if c != nil && c.Request != nil {
					if requestCtx := c.Request.Context(); requestCtx != nil {
						reqCtx = requestCtx
					}
				}
				if !managementasset.EnsureLatestManagementHTML(reqCtx, managementasset.StaticDir(s.configFilePath), cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository) {
					c.AbortWithStatus(http.StatusNotFound)
					return
				}
			} else {
				log.WithError(err).Error("failed to stat management control panel asset")
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
		}
		c.File(filePath)
		return
	}

	// SPA mode: try to serve the requested static file from panelDir first.
	// For example, /manage/assets/index-abc.js → panelDir/assets/index-abc.js
	reqPath := strings.TrimSpace(c.Param("filepath"))
	if reqPath != "" && reqPath != "/" {
		// Clean the path to prevent directory traversal.
		cleanPath := filepath.Clean(strings.TrimPrefix(reqPath, "/"))
		if cleanPath != "." && !strings.Contains(cleanPath, "..") {
			candidate := filepath.Join(panelDir, cleanPath)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				s.serveStaticFileWithCompression(c, candidate)
				return
			}
		}
	}

	// SPA fallback: serve manage.html for all non-static-asset routes.
	htmlFile := filepath.Join(panelDir, "manage.html")
	if _, err := os.Stat(htmlFile); err != nil {
		// Also try management.html for backwards compatibility.
		htmlFile = filepath.Join(panelDir, "management.html")
		if _, err = os.Stat(htmlFile); err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
	}
	// HTML files should not be cached – always serve fresh.
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.File(htmlFile)
}

func clearServerWriteDeadline(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
}

func (s *Server) resolvePanelDir() string {
	return managementasset.ResolvePanelDir(s.configFilePath)
}

// serveStaticFileWithCompression serves a static file with gzip compression
// (when the client supports it) and appropriate cache headers.
// Assets with content-hashed filenames (e.g. index-abc123.js) get immutable
// caching; all compressible types (JS, CSS, SVG, JSON) are gzip-encoded on the fly.
func (s *Server) serveStaticFileWithCompression(c *gin.Context, filePath string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	base := filepath.Base(filePath)

	// Set cache headers: hashed assets get long-lived immutable cache.
	// A filename is considered hashed if it contains a hyphen followed by
	// at least 6 alphanumeric characters before the extension, e.g. "index-DH6la3LJ.js".
	nameWithoutExt := strings.TrimSuffix(base, ext)
	if idx := strings.LastIndex(nameWithoutExt, "-"); idx > 0 && len(nameWithoutExt)-idx > 6 {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}

	// Determine if this file type benefits from compression.
	compressible := map[string]bool{
		".js": true, ".css": true, ".svg": true, ".json": true,
		".html": true, ".xml": true, ".txt": true, ".map": true,
	}

	if !compressible[ext] {
		c.File(filePath)
		return
	}

	// Check if the client accepts gzip encoding.
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.File(filePath)
		return
	}

	// Read the file and compress it.
	data, err := os.ReadFile(filePath)
	if err != nil {
		c.File(filePath)
		return
	}

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		c.File(filePath)
		return
	}
	if _, err = gz.Write(data); err != nil {
		_ = gz.Close()
		c.File(filePath)
		return
	}
	if err = gz.Close(); err != nil {
		c.File(filePath)
		return
	}

	// Only use compressed version if it's actually smaller.
	if buf.Len() >= len(data) {
		c.File(filePath)
		return
	}

	// Determine content type from extension.
	contentTypes := map[string]string{
		".js":   "application/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".svg":  "image/svg+xml",
		".json": "application/json; charset=utf-8",
		".html": "text/html; charset=utf-8",
		".xml":  "application/xml; charset=utf-8",
		".txt":  "text/plain; charset=utf-8",
		".map":  "application/json; charset=utf-8",
	}
	ct := contentTypes[ext]
	if ct == "" {
		ct = "application/octet-stream"
	}

	c.Header("Content-Encoding", "gzip")
	c.Header("Vary", "Accept-Encoding")
	c.Data(http.StatusOK, ct, buf.Bytes())
}

func (s *Server) enableKeepAlive(timeout time.Duration, onTimeout func()) {
	if timeout <= 0 || onTimeout == nil {
		return
	}

	s.keepAliveEnabled = true
	s.keepAliveTimeout = timeout
	s.keepAliveOnTimeout = onTimeout
	s.keepAliveHeartbeat = make(chan struct{}, 1)
	s.keepAliveStop = make(chan struct{}, 1)

	s.engine.GET("/keep-alive", s.handleKeepAlive)

	go s.watchKeepAlive()
}

func (s *Server) handleKeepAlive(c *gin.Context) {
	if s.localPassword != "" {
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		if provided != "" {
			parts := strings.SplitN(provided, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				provided = parts[1]
			}
		}
		if provided == "" {
			provided = strings.TrimSpace(c.GetHeader("X-Local-Password"))
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.localPassword)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
	}

	s.signalKeepAlive()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) signalKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}
	select {
	case s.keepAliveHeartbeat <- struct{}{}:
	default:
	}
}

func (s *Server) watchKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}

	timer := time.NewTimer(s.keepAliveTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			log.Warnf("keep-alive endpoint idle for %s, shutting down", s.keepAliveTimeout)
			if s.keepAliveOnTimeout != nil {
				s.keepAliveOnTimeout()
			}
			return
		case <-s.keepAliveHeartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.keepAliveTimeout)
		case <-s.keepAliveStop:
			return
		}
	}
}

// unifiedModelsHandler creates a unified handler for the /v1/models endpoint
// that routes to different handlers based on the User-Agent header.
// If User-Agent starts with "claude-cli", it routes to Claude handler,
// otherwise it routes to OpenAI handler.
// It also filters the returned models based on the API key's allowed-models restriction.
func (s *Server) unifiedModelsHandler(openaiHandler *openai.OpenAIAPIHandler, claudeHandler *claude.ClaudeCodeAPIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Check if this API key has allowed-models restriction
		var allowedModels map[string]struct{}
		var allowedChannels map[string]struct{}
		allowedChannelGroups := allowedChannelGroupsFromAccessMetadata(c)
		routeCtx := pathRouteContextFromGin(c)
		routeGroup := ""
		if routeCtx != nil {
			routeGroup = routeCtx.Group
		}
		if metadataVal, exists := c.Get("accessMetadata"); exists {
			if metadata, ok := metadataVal.(map[string]string); ok {
				if allowedStr, exists := metadata["allowed-models"]; exists && allowedStr != "" {
					allowedModels = make(map[string]struct{})
					for _, m := range strings.Split(allowedStr, ",") {
						trimmed := strings.TrimSpace(m)
						if trimmed != "" {
							allowedModels[trimmed] = struct{}{}
						}
					}
					if len(allowedModels) == 0 {
						allowedModels = nil
					}
				}
				if allowedStr, exists := metadata["allowed-channels"]; exists && allowedStr != "" {
					allowedChannels = make(map[string]struct{})
					for _, channel := range strings.Split(allowedStr, ",") {
						trimmed := strings.ToLower(strings.TrimSpace(channel))
						if trimmed != "" {
							allowedChannels[trimmed] = struct{}{}
						}
					}
					if len(allowedChannels) == 0 {
						allowedChannels = nil
					}
				}
			}
		}

		// If no restriction, just call the handler directly
		if allowedModels == nil && allowedChannels == nil && allowedChannelGroups == nil && routeGroup == "" {
			userAgent := c.GetHeader("User-Agent")
			if strings.HasPrefix(userAgent, "claude-cli") {
				claudeHandler.ClaudeModels(c)
			} else {
				openaiHandler.OpenAIModels(c)
			}
			return
		}

		// With restriction: capture the response, filter, then write
		recorder := &responseRecorder{
			ResponseWriter: c.Writer,
			body:           &bytes.Buffer{},
		}
		c.Writer = recorder

		userAgent := c.GetHeader("User-Agent")
		if strings.HasPrefix(userAgent, "claude-cli") {
			claudeHandler.ClaudeModels(c)
		} else {
			openaiHandler.OpenAIModels(c)
		}

		// Parse and filter the captured response
		var resp struct {
			Object string                   `json:"object"`
			Data   []map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(recorder.body.Bytes(), &resp); err != nil {
			// If parsing fails, just write the original response
			recorder.ResponseWriter.WriteHeader(recorder.statusCode)
			_, _ = recorder.ResponseWriter.Write(recorder.body.Bytes())
			return
		}

		// Filter models
		filtered := make([]map[string]interface{}, 0, len(resp.Data))
		for _, model := range resp.Data {
			if id, ok := model["id"].(string); ok {
				if allowedModels != nil {
					if _, allowed := allowedModels[id]; !allowed {
						continue
					}
				}
				if allowedChannels != nil || allowedChannelGroups != nil || routeGroup != "" {
					if s.handlers == nil || s.handlers.AuthManager == nil || !s.handlers.AuthManager.CanServeModelWithScopes(id, allowedChannels, allowedChannelGroups, routeGroup) {
						continue
					}
				}
				filtered = append(filtered, model)
			}
		}
		resp.Data = filtered

		// Write filtered response
		filteredJSON, err := json.Marshal(resp)
		if err != nil {
			recorder.ResponseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}

		recorder.ResponseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")
		recorder.ResponseWriter.Header().Set("Content-Length", fmt.Sprintf("%d", len(filteredJSON)))
		recorder.ResponseWriter.WriteHeader(http.StatusOK)
		_, _ = recorder.ResponseWriter.Write(filteredJSON)
	}
}

// responseRecorder captures the response body for post-processing
type responseRecorder struct {
	gin.ResponseWriter
	body       *bytes.Buffer
	statusCode int
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func (r *responseRecorder) WriteHeader(code int) {
	r.statusCode = code
}

// Start begins listening for and serving HTTP or HTTPS requests.
// It's a blocking call and will only return on an unrecoverable error.
//
// Returns:
//   - error: An error if the server fails to start
func (s *Server) Start() error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}

	useTLS := s.cfg != nil && s.cfg.TLS.Enable
	if useTLS {
		cert := strings.TrimSpace(s.cfg.TLS.Cert)
		key := strings.TrimSpace(s.cfg.TLS.Key)
		if cert == "" || key == "" {
			return fmt.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
		}
		log.Debugf("Starting API server on %s with TLS", s.server.Addr)
		if errServeTLS := s.server.ListenAndServeTLS(cert, key); errServeTLS != nil && !errors.Is(errServeTLS, http.ErrServerClosed) {
			return fmt.Errorf("failed to start HTTPS server: %v", errServeTLS)
		}
		return nil
	}

	log.Debugf("Starting API server on %s", s.server.Addr)
	if errServe := s.server.ListenAndServe(); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
		return fmt.Errorf("failed to start HTTP server: %v", errServe)
	}

	return nil
}

// Stop gracefully shuts down the API server without interrupting any
// active connections.
//
// Parameters:
//   - ctx: The context for graceful shutdown
//
// Returns:
//   - error: An error if the server fails to stop
func (s *Server) Stop(ctx context.Context) error {
	log.Debug("Stopping API server...")

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	if s.mgmt != nil {
		s.mgmt.Close()
	}

	// Shutdown the HTTP server.
	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %v", err)
	}

	log.Debug("API server stopped")
	return nil
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware(cfgProvider func() *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Management APIs and the embedded panel should not be callable cross-origin by default.
		// The panel is served from the same origin, so it does not need wildcard CORS.
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
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "origin not allowed"})
			return
		}

		c.Header("Vary", "Origin")
		c.Header("Access-Control-Allow-Origin", allowedOrigin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Origin")
		c.Header("Access-Control-Expose-Headers", "X-CPA-VERSION, X-CPA-COMMIT, X-CPA-BUILD-DATE")

		if c.Request.Method == "OPTIONS" {
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

	if isChromeExtensionOrigin(origin) {
		return origin
	}

	if cfg == nil {
		return ""
	}

	for _, candidate := range cfg.CORSAllowOrigins {
		if strings.EqualFold(strings.TrimSpace(candidate), origin) {
			return origin
		}
	}

	return ""
}

func isChromeExtensionOrigin(origin string) bool {
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" {
		return false
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil {
		return false
	}

	if !strings.EqualFold(parsed.Scheme, "chrome-extension") {
		return false
	}

	return strings.TrimSpace(parsed.Host) != ""
}

// versionHeaderMiddleware returns a Gin middleware handler that adds version
// headers to every response, allowing the frontend to display the backend version.
//
// Returns:
//   - gin.HandlerFunc: The version header middleware handler
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

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return
	}
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	if len(s.oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}

	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}

	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second)
	}

	// Update log level dynamically when debug flag changes
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}

	s.applyAccessConfig(oldCfg, cfg)
	s.cfg = cfg
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	managementasset.SetCurrentConfig(cfg)
	// Save YAML snapshot for next comparison
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	s.handlers.UpdateClients(&cfg.SDKConfig)

	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
	}

	// Notify Amp module only when Amp config has changed.
	ampConfigChanged := oldCfg == nil || !reflect.DeepEqual(oldCfg.AmpCode, cfg.AmpCode)
	if ampConfigChanged {
		if s.ampModule != nil {
			log.Debugf("triggering amp module config update")
			if err := s.ampModule.OnConfigUpdated(cfg); err != nil {
				log.Errorf("failed to update Amp module config: %v", err)
			}
		} else {
			log.Warnf("amp module is nil, skipping config update")
		}
	}

	// Count client sources from configuration and auth store.
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	// Counting auth entries is a config-application bookkeeping step that is not
	// tied to any request lifecycle. It intentionally uses a root context and
	// degrades to zero on listing failure inside util.CountAuthFiles.
	authEntries := util.CountAuthFiles(context.Background(), tokenStore)
	geminiAPIKeyCount := len(cfg.GeminiKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		openAICompatCount += len(entry.APIKeyEntries)
	}

	total := authEntries + geminiAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + vertexAICompatCount + openAICompatCount
	fmt.Printf("server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Claude API keys + %d Codex keys + %d Vertex-compat + %d OpenAI-compat)\n",
		total,
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil {
		return
	}
	s.wsAuthChanged = fn
}

// (management handlers moved to internal/api/handlers/management)

// AuthMiddleware returns a Gin middleware handler that authenticates requests
// using the configured authentication providers.
func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			// This should never happen in a properly initialized server.
			// Failing closed prevents accidentally exposing a public proxy.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "authentication manager not initialized"})
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("apiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

// ModelRestrictionMiddleware enforces the allowed-models restriction from API key config.
// It reads the "allowed-models" metadata set by AuthMiddleware, parses the model from
// the request body, and returns 403 if the model is not in the allowed list.
func ModelRestrictionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only check POST requests (GET /models etc. don't need restriction)
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Get allowed-models from auth metadata
		metadataVal, exists := c.Get("accessMetadata")
		if !exists {
			c.Next()
			return
		}
		metadata, ok := metadataVal.(map[string]string)
		if !ok {
			c.Next()
			return
		}
		allowedStr, exists := metadata["allowed-models"]
		if !exists || allowedStr == "" {
			// No restriction — allow all models
			c.Next()
			return
		}

		// Parse allowed models into a set
		allowedModels := make(map[string]struct{})
		for _, m := range strings.Split(allowedStr, ",") {
			trimmed := strings.TrimSpace(m)
			if trimmed != "" {
				allowedModels[trimmed] = struct{}{}
			}
		}
		if len(allowedModels) == 0 {
			c.Next()
			return
		}

		// Read the body to extract the model field
		bodyBytes, err := bodyutil.ReadRequestBody(c, bodyutil.DefaultRequestBodyLimit)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			c.Next()
			return
		}

		// Extract model field from JSON
		var bodyObj struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(bodyBytes, &bodyObj); err != nil || bodyObj.Model == "" {
			// Can't parse model — let downstream handle it
			c.Next()
			return
		}

		// Check if model is allowed
		if _, allowed := allowedModels[bodyObj.Model]; !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": map[string]interface{}{
					"message": fmt.Sprintf("model '%s' is not allowed for this API key", bodyObj.Model),
					"type":    "forbidden",
					"code":    "model_not_allowed",
				},
			})
			return
		}

		c.Next()
	}
}

// SystemPromptMiddleware injects a system-prompt (from API key config) into
// POST request bodies. It supports two formats:
//   - OpenAI Chat Completions / Claude: prepends a system message to "messages"
//   - OpenAI Responses API: prepends to the "instructions" field
func SystemPromptMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		metadataVal, exists := c.Get("accessMetadata")
		if !exists {
			c.Next()
			return
		}
		metadata, ok := metadataVal.(map[string]string)
		if !ok {
			c.Next()
			return
		}
		systemPrompt, exists := metadata["system-prompt"]
		if !exists || strings.TrimSpace(systemPrompt) == "" {
			c.Next()
			return
		}

		// Read body
		bodyBytes, err := bodyutil.ReadRequestBody(c, bodyutil.DefaultRequestBodyLimit)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			c.Next()
			return
		}

		var newBody []byte

		// Try Chat Completions format first (has "messages" array)
		if gjson.GetBytes(bodyBytes, "messages").Exists() && gjson.GetBytes(bodyBytes, "messages").IsArray() {
			// Build system message JSON and prepend to messages array
			sysMsg := map[string]interface{}{
				"role":    "system",
				"content": systemPrompt,
			}
			var body map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				c.Next()
				return
			}
			messages, _ := body["messages"].([]interface{})
			newMessages := make([]interface{}, 0, len(messages)+1)
			newMessages = append(newMessages, sysMsg)
			newMessages = append(newMessages, messages...)
			body["messages"] = newMessages
			newBody, err = json.Marshal(body)
			if err != nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				c.Next()
				return
			}
			log.Debugf("[SystemPrompt] injected into messages (count: %d→%d)", len(messages), len(newMessages))

		} else if gjson.GetBytes(bodyBytes, "input").Exists() {
			// Responses API format: inject into "instructions" field
			existing := strings.TrimSpace(gjson.GetBytes(bodyBytes, "instructions").String())
			var combined string
			if existing != "" {
				combined = systemPrompt + "\n\n" + existing
			} else {
				combined = systemPrompt
			}
			newBody, _ = sjson.SetBytes(bodyBytes, "instructions", combined)
			if newBody == nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				c.Next()
				return
			}
			log.Debugf("[SystemPrompt] injected into instructions (Responses API)")

		} else {
			// Unknown format — pass through
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			c.Next()
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(newBody))
		c.Request.ContentLength = int64(len(newBody))
		c.Next()
	}
}

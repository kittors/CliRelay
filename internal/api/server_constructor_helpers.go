package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/modules"
	ampmodule "github.com/router-for-me/CLIProxyAPI/v6/internal/api/modules/amp"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func newServerOptionState(opts []ServerOption) *serverOptionConfig {
	optionState := &serverOptionConfig{
		requestLoggerFactory: defaultRequestLoggerFactory,
	}
	for i := range opts {
		opts[i](optionState)
	}
	return optionState
}

func configureServerMode(cfg *config.Config) {
	if cfg != nil && !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
}

func newServerEngine(cfg *config.Config, optionState *serverOptionConfig) *gin.Engine {
	engine := gin.New()
	applyBodyUtilRequestBodyConfig(cfg)
	if err := bodyutil.CleanupOldRequestBodyCacheFiles(5 * time.Minute); err != nil {
		log.Warnf("failed to cleanup request body cache files: %v", err)
	}
	if cfg != nil {
		configureTrustedProxies(engine, cfg.TrustedProxies)
	}
	if optionState != nil && optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
	engine.Use(middleware.DecompressRequestMiddleware())
	engine.Use(middleware.RequestBodyCleanupMiddleware())
	if optionState != nil {
		for _, mw := range optionState.extraMiddleware {
			engine.Use(mw)
		}
	}
	return engine
}

func configureRequestLoggerMiddleware(
	engine *gin.Engine,
	cfg *config.Config,
	configFilePath string,
	optionState *serverOptionConfig,
) (logging.RequestLogger, func(bool)) {
	if engine == nil || cfg == nil || cfg.CommercialMode {
		return nil, nil
	}
	if optionState == nil || optionState.requestLoggerFactory == nil {
		return nil, nil
	}

	requestLogger := optionState.requestLoggerFactory(cfg, configFilePath)
	if requestLogger == nil {
		return nil, nil
	}
	engine.Use(middleware.RequestLoggingMiddleware(requestLogger))

	var toggle func(bool)
	if setter, ok := requestLogger.(interface{ SetEnabled(bool) }); ok {
		toggle = setter.SetEnabled
	}
	return requestLogger, toggle
}

func newServerRuntimeState(
	engine *gin.Engine,
	cfg *config.Config,
	authManager *auth.Manager,
	accessManager *sdkaccess.Manager,
	configFilePath string,
	requestLogger logging.RequestLogger,
	loggerToggle func(bool),
) *Server {
	currentPath, envManagementSecret := resolveServerEnvironment(configFilePath)
	return &Server{
		engine:              engine,
		handlers:            handlers.NewBaseAPIHandlers(&cfg.SDKConfig, authManager),
		cfg:                 cfg,
		accessManager:       accessManager,
		requestLogger:       requestLogger,
		loggerToggle:        loggerToggle,
		configFilePath:      configFilePath,
		currentPath:         currentPath,
		envManagementSecret: envManagementSecret,
		wsRoutes:            make(map[string]struct{}),
	}
}

func resolveServerEnvironment(configFilePath string) (currentPath string, envManagementSecret bool) {
	currentPath, err := os.Getwd()
	if err != nil {
		currentPath = configFilePath
	}
	envAdminPassword, envAdminPasswordSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envAdminPassword = strings.TrimSpace(envAdminPassword)
	return currentPath, envAdminPasswordSet && envAdminPassword != ""
}

func (s *Server) installDynamicMiddleware(configFilePath string) {
	if s == nil || s.engine == nil {
		return
	}
	s.engine.Use(func(c *gin.Context) {
		s.inFlightRequests.Add(1)
		defer s.inFlightRequests.Add(-1)
		c.Next()
	})
	s.engine.Use(corsMiddleware(func() *config.Config {
		if s.cfg != nil {
			return s.cfg
		}
		return nil
	}))
	s.engine.Use(versionHeaderMiddleware(configFilePath))
}

func (s *Server) applyInitialRuntimeConfig(cfg *config.Config, authManager *auth.Manager) {
	if s == nil || cfg == nil {
		return
	}
	applyBodyUtilRequestBodyConfig(cfg)
	if s.handlers != nil {
		s.handlers.AuthManager = authManager
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
	s.applyAccessConfig(nil, cfg)
	if authManager != nil {
		authManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second)
	}
	managementasset.SetCurrentConfig(cfg)
	auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	s.applyProxyWarmupConfig(cfg)
}

func configModelRequestBodyLimitBytes(cfg *config.Config) int64 {
	if cfg == nil {
		return int64(config.DefaultModelRequestBodyMB) << 20
	}
	return cfg.ModelRequestBodyLimitBytes()
}

func configRequestBodyDiskThresholdBytes(cfg *config.Config) int64 {
	if cfg == nil {
		return int64(config.DefaultRequestBodyDiskThresholdMB) << 20
	}
	return cfg.RequestBodyDiskThresholdBytes()
}

func applyBodyUtilRequestBodyConfig(cfg *config.Config) {
	bodyutil.SetModelRequestBodyLimit(configModelRequestBodyLimitBytes(cfg))
	bodyutil.SetRequestBodyDiskThreshold(configRequestBodyDiskThresholdBytes(cfg))
	if cfg == nil || cfg.RequestBodyCacheDir() == "" {
		bodyutil.ResetRequestBodyCacheDir()
		return
	}
	bodyutil.SetRequestBodyCacheDir(cfg.RequestBodyCacheDir())
}

func (s *Server) configureManagementHandler(
	cfg *config.Config,
	configFilePath string,
	authManager *auth.Manager,
	accessManager *sdkaccess.Manager,
	optionState *serverOptionConfig,
) {
	if s == nil {
		return
	}
	s.mgmt = managementHandlers.NewHandler(cfg, configFilePath, authManager)
	s.mgmt.SetAccessManager(accessManager)
	if optionState != nil && optionState.configMutatedCallback != nil {
		s.mgmt.SetConfigMutatedHook(optionState.configMutatedCallback)
	} else {
		s.mgmt.SetConfigMutatedHook(s.buildDefaultConfigMutatedHook(cfg, configFilePath))
	}
	if optionState != nil && optionState.localPassword != "" {
		s.mgmt.SetLocalPassword(optionState.localPassword)
		s.localPassword = optionState.localPassword
	}
	s.mgmt.SetLogDirectory(logging.ResolveLogDirectory(cfg))
	if optionState != nil && optionState.postAuthHook != nil {
		s.mgmt.SetPostAuthHook(optionState.postAuthHook)
	}
	if optionState != nil && optionState.modelConfigMutatedCallback != nil {
		s.mgmt.SetModelConfigMutatedHook(optionState.modelConfigMutatedCallback)
	}
}

func (s *Server) registerBuiltinModules(cfg *config.Config, accessManager *sdkaccess.Manager) {
	if s == nil || s.engine == nil || cfg == nil {
		return
	}
	s.ampModule = ampmodule.NewLegacy(accessManager, AuthMiddleware(accessManager))
	ctx := modules.Context{
		Engine:         s.engine,
		BaseHandler:    s.handlers,
		Config:         cfg,
		AuthMiddleware: AuthMiddleware(accessManager),
	}
	if err := modules.RegisterModule(ctx, s.ampModule); err != nil {
		log.Errorf("Failed to register Amp module: %v", err)
	}
}

func (s *Server) applyRouterConfigurator(optionState *serverOptionConfig, cfg *config.Config) {
	if s == nil || s.engine == nil || s.handlers == nil || optionState == nil || optionState.routerConfigurator == nil {
		return
	}
	optionState.routerConfigurator(s.engine, s.handlers, cfg)
}

func (s *Server) configureInitialManagementRoutes(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || s.envManagementSecret || s.localPassword != ""
	hasIdentityAuth := identity.Default() != nil
	s.managementRoutesEnabled.Store(hasManagementSecret || hasIdentityAuth)
	if hasManagementSecret || hasIdentityAuth {
		s.registerManagementRoutes()
	}
}

func (s *Server) configureInitialKeepAlive(optionState *serverOptionConfig) {
	if s == nil || optionState == nil || !optionState.keepAliveEnabled {
		return
	}
	s.enableKeepAlive(optionState.keepAliveTimeout, optionState.keepAliveOnTimeout)
}

func buildHTTPServer(cfg *config.Config, engine *gin.Engine) *http.Server {
	readTimeout := config.DefaultMainAPIReadTimeout
	host := ""
	port := 8315
	if cfg != nil {
		readTimeout = cfg.MainAPIReadTimeout()
		host = strings.TrimSpace(cfg.Host)
		if cfg.Port > 0 {
			port = cfg.Port
		}
	}
	return &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           engine,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       readTimeout,
		WriteTimeout:      mainAPIServerWriteTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
}

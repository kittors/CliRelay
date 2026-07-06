// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

// Service wraps the proxy server lifecycle so external programs can embed the CLI proxy.
// It manages the complete lifecycle including authentication, file watching, HTTP server,
// and integration with various AI service providers.
type Service struct {
	// cfg holds the current application configuration.
	cfg *config.Config

	// cfgMu protects concurrent access to the configuration.
	cfgMu sync.RWMutex

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption

	// server is the HTTP API server instance.
	server *api.Server

	// pprofServer manages the optional pprof HTTP debug server.
	pprofServer pprofRuntime

	// serverErr channel for server startup/shutdown errors.
	serverErr chan error

	// watcher handles file system monitoring.
	watcher *WatcherWrapper

	// watcherCancel cancels the watcher context.
	watcherCancel context.CancelFunc

	// authUpdates channel for authentication updates.
	authUpdates chan runtimeAuthUpdate

	// authQueueStop cancels the auth update queue processing.
	authQueueStop context.CancelFunc

	// authQueueWG waits for the service-owned auth update queue goroutine.
	authQueueWG sync.WaitGroup

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// shutdownOnce ensures shutdown is called only once.
	shutdownOnce sync.Once

	// wsGateway manages websocket Gemini providers.
	wsGateway websocketGateway
}

// Builder constructs a Service instance with customizable providers.
// It provides a fluent interface for configuring all aspects of the service
// including authentication, file watching, HTTP server options, and lifecycle hooks.
type Builder struct {
	// cfg holds the application configuration.
	cfg *config.Config

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// serverOptions contains additional server configuration options.
	serverOptions []api.ServerOption
}

// Hooks allows callers to plug into service lifecycle stages.
// These callbacks provide opportunities to perform custom initialization
// and cleanup operations during service startup and shutdown.
type Hooks struct {
	// OnBeforeStart is called before the service starts, allowing configuration
	// modifications or additional setup.
	OnBeforeStart func(*config.Config)

	// OnAfterStart is called after the service has started successfully,
	// providing access to the service instance for additional operations.
	OnAfterStart func(*Service)
}

// NewBuilder creates a Builder with default dependencies left unset.
// Use the fluent interface methods to configure the service before calling Build().
func NewBuilder() *Builder {
	return &Builder{}
}

// WithConfig sets the configuration instance used by the service.
func (b *Builder) WithConfig(cfg *config.Config) *Builder {
	b.cfg = cfg
	return b
}

// WithConfigPath sets the absolute configuration file path used for reload watching.
func (b *Builder) WithConfigPath(path string) *Builder {
	b.configPath = path
	return b
}

// WithTokenClientProvider overrides the provider responsible for token-backed clients.
func (b *Builder) WithTokenClientProvider(provider TokenClientProvider) *Builder {
	b.tokenProvider = provider
	return b
}

// WithAPIKeyClientProvider overrides the provider responsible for API key-backed clients.
func (b *Builder) WithAPIKeyClientProvider(provider APIKeyClientProvider) *Builder {
	b.apiKeyProvider = provider
	return b
}

// WithWatcherFactory allows customizing the watcher factory that handles reloads.
func (b *Builder) WithWatcherFactory(factory WatcherFactory) *Builder {
	b.watcherFactory = factory
	return b
}

// WithHooks registers lifecycle hooks executed around service startup.
func (b *Builder) WithHooks(h Hooks) *Builder {
	b.hooks = h
	return b
}

// WithAuthManager overrides the authentication manager used for token lifecycle operations.
func (b *Builder) WithAuthManager(mgr *sdkAuth.Manager) *Builder {
	b.authManager = mgr
	return b
}

// WithRequestAccessManager overrides the request authentication manager.
func (b *Builder) WithRequestAccessManager(mgr *sdkaccess.Manager) *Builder {
	b.accessManager = mgr
	return b
}

// WithCoreAuthManager overrides the runtime auth manager responsible for request execution.
func (b *Builder) WithCoreAuthManager(mgr *coreauth.Manager) *Builder {
	b.coreManager = mgr
	return b
}

// WithServerOptions appends server configuration options used during construction.
func (b *Builder) WithServerOptions(opts ...api.ServerOption) *Builder {
	b.serverOptions = append(b.serverOptions, opts...)
	return b
}

// WithLocalManagementPassword configures a password that is only accepted from localhost management requests.
func (b *Builder) WithLocalManagementPassword(password string) *Builder {
	if password == "" {
		return b
	}
	b.serverOptions = append(b.serverOptions, api.WithLocalManagementPassword(password))
	return b
}

// WithPostAuthHook registers a hook to be called after an Auth record is created
// but before it is persisted to storage.
func (b *Builder) WithPostAuthHook(hook coreauth.PostAuthHook) *Builder {
	if hook == nil {
		return b
	}
	b.serverOptions = append(b.serverOptions, api.WithPostAuthHook(hook))
	return b
}

// Build validates inputs, applies defaults, and returns a ready-to-run service.
func (b *Builder) Build() (*Service, error) {
	if b.cfg == nil {
		return nil, fmt.Errorf("cliproxy: configuration is required")
	}
	if b.configPath == "" {
		return nil, fmt.Errorf("cliproxy: configuration path is required")
	}

	tokenProvider := b.tokenProvider
	if tokenProvider == nil {
		tokenProvider = NewFileTokenClientProvider()
	}

	apiKeyProvider := b.apiKeyProvider
	if apiKeyProvider == nil {
		apiKeyProvider = NewAPIKeyClientProvider()
	}

	watcherFactory := b.watcherFactory
	if watcherFactory == nil {
		watcherFactory = defaultWatcherFactory
	}

	authManager := b.authManager
	if authManager == nil {
		authManager = newDefaultAuthManager()
	}

	accessManager := b.accessManager
	if accessManager == nil {
		accessManager = sdkaccess.NewManager()
	}
	accessManager.SetAllowAllWhenNoProviders(b.cfg.SDKConfig.AllowUnauthenticated)
	if b.cfg.SDKConfig.AllowUnauthenticated {
		log.Warn("security warning: allow-unauthenticated is enabled; /v1 and /v1beta will be publicly accessible when no API keys are configured")
	}

	configureServiceAccess(b.cfg, accessManager)

	coreManager := b.coreManager
	if coreManager == nil {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && b.cfg != nil {
			dirSetter.SetBaseDir(b.cfg.AuthDir)
		}

		strategy := ""
		if b.cfg != nil {
			strategy = strings.ToLower(strings.TrimSpace(b.cfg.Routing.Strategy))
		}
		var selector coreauth.Selector
		switch strategy {
		case "fill-first", "fillfirst", "ff":
			selector = &coreauth.FillFirstSelector{}
		default:
			selector = &coreauth.RoundRobinSelector{}
		}

		coreManager = coreauth.NewManager(tokenStore, selector, nil)
	}
	// Attach a default RoundTripper provider so providers can opt-in per-auth transports.
	coreManager.SetRoundTripperProvider(newDefaultRoundTripperProvider())
	coreManager.SetConfig(b.cfg)
	coreManager.SetOAuthModelAlias(b.cfg.OAuthModelAlias)

	service := &Service{
		cfg:            b.cfg,
		configPath:     b.configPath,
		tokenProvider:  tokenProvider,
		apiKeyProvider: apiKeyProvider,
		watcherFactory: watcherFactory,
		hooks:          b.hooks,
		authManager:    authManager,
		accessManager:  accessManager,
		coreManager:    coreManager,
		serverOptions:  append([]api.ServerOption(nil), b.serverOptions...),
	}
	return service, nil
}

// RegisterUsagePlugin registers a usage plugin on the global usage manager.
func (s *Service) RegisterUsagePlugin(plugin usage.Plugin) {
	usage.RegisterPlugin(plugin)
}

// NewFileTokenClientProvider returns the default token-backed client loader.
func NewFileTokenClientProvider() TokenClientProvider {
	return &fileTokenClientProvider{}
}

type fileTokenClientProvider struct{}

func (p *fileTokenClientProvider) Load(ctx context.Context, cfg *config.Config) (*TokenClientResult, error) {
	// Stateless executors handle tokens.
	_ = ctx
	_ = cfg
	return &TokenClientResult{SuccessfulAuthed: 0}, nil
}

// NewAPIKeyClientProvider returns the default API key client loader that reuses existing logic.
func NewAPIKeyClientProvider() APIKeyClientProvider {
	return &apiKeyClientProvider{}
}

type apiKeyClientProvider struct{}

func (p *apiKeyClientProvider) Load(ctx context.Context, cfg *config.Config) (*APIKeyClientResult, error) {
	geminiCount, vertexCompatCount, claudeCount, codexCount, bedrockCount, openCodeGoCount, ollamaCloudCount, openAICompat := buildAPIKeyClients(cfg)
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	return &APIKeyClientResult{
		GeminiKeyCount:       geminiCount,
		VertexCompatKeyCount: vertexCompatCount,
		ClaudeKeyCount:       claudeCount,
		CodexKeyCount:        codexCount,
		BedrockKeyCount:      bedrockCount,
		OpenCodeGoKeyCount:   openCodeGoCount,
		OllamaCloudKeyCount:  ollamaCloudCount,
		OpenAICompatCount:    openAICompat,
	}, nil
}

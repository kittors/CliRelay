// Package cmd provides command-line interface functionality for the CLI Proxy API server.
// It includes authentication flows for various AI service providers, service startup,
// and other command-line operations.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy"
	log "github.com/sirupsen/logrus"
)

// StartService builds and runs the proxy service using the exported SDK.
// It creates a new proxy service instance, sets up signal handling for graceful shutdown,
// and starts the service with the provided configuration.
//
// Parameters:
//   - cfg: The application configuration
//   - configPath: The path to the configuration file
//   - localPassword: Optional password accepted for local management requests
func StartService(cfg *config.Config, configPath string, localPassword string) {
	loc := config.ApplyTimeZone(cfg.Timezone)
	if err := initializeRuntimeDataStack(cfg, configPath, loc); err != nil {
		log.Errorf("usage: failed to initialize runtime data stack: %v", err)
		return
	}
	usage.InitRedis(cfg.Redis)
	defer usage.StopRedis()

	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword)

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	runCtx := ctxSignal
	if localPassword != "" {
		var keepAliveCancel context.CancelFunc
		runCtx, keepAliveCancel = context.WithCancel(ctxSignal)
		builder = builder.WithServerOptions(api.WithKeepAliveEndpoint(10*time.Second, func() {
			log.Warn("keep-alive endpoint idle for 10s, shutting down")
			keepAliveCancel()
		}))
	}

	service, err := builder.Build()
	if err != nil {
		log.Errorf("failed to build proxy service: %v", err)
		return
	}

	err = service.Run(runCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("proxy service exited with error: %v", err)
	}
}

// StartServiceBackground starts the proxy service in a background goroutine
// and returns a cancel function for shutdown and a done channel.
func StartServiceBackground(cfg *config.Config, configPath string, localPassword string) (cancel func(), done <-chan struct{}) {
	loc := config.ApplyTimeZone(cfg.Timezone)
	if err := initializeRuntimeDataStack(cfg, configPath, loc); err != nil {
		log.Errorf("usage: failed to initialize runtime data stack: %v", err)
		doneCh := make(chan struct{})
		close(doneCh)
		return func() {}, doneCh
	}
	usage.InitRedis(cfg.Redis)

	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithLocalManagementPassword(localPassword)

	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	service, err := builder.Build()
	if err != nil {
		log.Errorf("failed to build proxy service: %v", err)
		usage.StopRedis()
		close(doneCh)
		return cancelFn, doneCh
	}

	go func() {
		defer close(doneCh)
		defer usage.StopRedis()
		if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Errorf("proxy service exited with error: %v", err)
		}
	}()

	return cancelFn, doneCh
}

func initializeRuntimeDataStack(cfg *config.Config, configPath string, loc *time.Location) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if err := usage.InitPostgres(cfg.Postgres, cfg.RequestLogStorage, loc); err != nil {
		return err
	}
	bootstrapPassword := strings.TrimSpace(os.Getenv("CLIRELAY_ADMIN_PASSWORD"))
	if bootstrapPassword == "" {
		bootstrapPassword = strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD"))
	}
	if bootstrapPassword == "" {
		bootstrapPassword = strings.TrimSpace(cfg.RemoteManagement.SecretKey)
	}
	identityService := identity.NewService(usage.RuntimeDB())
	enduserService := enduser.NewService(usage.RuntimeDB())
	enduser.SetDefault(enduserService)
	if err := identityService.Bootstrap(context.Background(), bootstrapPassword); err != nil {
		return fmt.Errorf("identity bootstrap: %w", err)
	}
	identity.SetDefault(identityService)
	// Import YAML keys first so one-shot end-user backfill can see them.
	if _, err := usage.MigrateAPIKeysFromConfig(cfg, configPath); err != nil {
		return fmt.Errorf("migrate api keys from config: %w", err)
	}
	if created, err := enduserService.BackfillFromAPIKeys(context.Background()); err != nil {
		log.WithError(err).Error("enduser: backfill from api keys failed")
		return fmt.Errorf("enduser backfill: %w", err)
	} else if created > 0 {
		log.Infof("enduser: backfilled %d end users from api keys", created)
	}
	usage.MigrateAPIKeyPermissionProfilesFromYAML(configPath)
	usage.MigrateRoutingConfigFromConfig(cfg, configPath)
	usage.ApplyStoredRoutingConfig(cfg)
	usage.MigrateProxyPoolFromConfig(cfg, configPath)
	usage.ApplyStoredProxyPool(cfg)
	settingsstore.MigrateRuntimeSettingsFromConfig(cfg, configPath)
	settingsstore.ApplyStoredRuntimeSettings(cfg)
	middleware.InitQuotaUsageFuncs(usage.CountTodayByKey, usage.CountTotalByKey, usage.QueryTotalCostByKey, usage.QueryTodayCostByKey)
	usage.SetTokenUsageCallback(middleware.RecordTokenUsage)
	return nil
}

// WaitForCloudDeploy waits indefinitely for shutdown signals in cloud deploy mode
// when no configuration file is available.
func WaitForCloudDeploy() {
	// Clarify that we are intentionally idle for configuration and not running the API server.
	log.Info("Cloud deploy mode: No config found; standing by for configuration. API server is not started. Press Ctrl+C to exit.")

	ctxSignal, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Block until shutdown signal is received
	<-ctxSignal.Done()
	log.Info("Cloud deploy mode: Shutdown signal received; exiting")
}

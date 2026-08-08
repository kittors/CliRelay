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
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
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
	defer stopRuntimeDataStack()

	moderator := contentmoderation.NewRequestModerator(contentmoderation.NewStore(usage.RuntimeDB()), contentmoderation.NewEvaluator(nil))
	contentmoderation.SetRuntime(moderator)
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithCoreAuthHook(usage.NewAIAccountBindingHook()).
		WithRequestModerator(moderator).
		WithHooks(runtimeDataStackPostStartHooks(defaultRuntimeDataStackMaintenanceOps())).
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

	moderator := contentmoderation.NewRequestModerator(contentmoderation.NewStore(usage.RuntimeDB()), contentmoderation.NewEvaluator(nil))
	contentmoderation.SetRuntime(moderator)
	builder := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithCoreAuthHook(usage.NewAIAccountBindingHook()).
		WithRequestModerator(moderator).
		WithHooks(runtimeDataStackPostStartHooks(defaultRuntimeDataStackMaintenanceOps())).
		WithLocalManagementPassword(localPassword)

	ctx, cancelFn := context.WithCancel(context.Background())
	doneCh := make(chan struct{})

	service, err := builder.Build()
	if err != nil {
		log.Errorf("failed to build proxy service: %v", err)
		stopRuntimeDataStack()
		close(doneCh)
		return cancelFn, doneCh
	}

	go func() {
		defer close(doneCh)
		defer stopRuntimeDataStack()
		if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Errorf("proxy service exited with error: %v", err)
		}
	}()

	return cancelFn, doneCh
}

// sessionReaperStop holds the identity session reaper's cancel function. The
// reaper is started with the runtime data stack, so it has to be torn down with
// it; a process that re-initialises the stack (tests, cloud-deploy reconfigure)
// would otherwise leak one goroutine and one ticker per initialisation.
var (
	sessionReaperMu   sync.Mutex
	sessionReaperStop func()
)

func startSessionReaper(service *identity.Service, interval time.Duration) {
	sessionReaperMu.Lock()
	defer sessionReaperMu.Unlock()
	if sessionReaperStop != nil {
		sessionReaperStop()
	}
	sessionReaperStop = service.StartSessionReaper(context.Background(), interval)
}

// auditRetentionStop mirrors the session reaper's lifecycle: started with the
// runtime data stack, so it has to be stopped with it or a re-initialisation
// leaks a goroutine and a ticker.
var (
	auditRetentionMu   sync.Mutex
	auditRetentionStop func()
)

func startAuditRetention(service *identity.Service, policy identity.AuditRetentionPolicy) {
	auditRetentionMu.Lock()
	defer auditRetentionMu.Unlock()
	if auditRetentionStop != nil {
		auditRetentionStop()
	}
	auditRetentionStop = service.StartAuditRetention(context.Background(), policy)
}

// stopRuntimeDataStack tears down everything initializeRuntimeDataStack started.
func stopRuntimeDataStack() {
	sessionReaperMu.Lock()
	stop := sessionReaperStop
	sessionReaperStop = nil
	sessionReaperMu.Unlock()
	if stop != nil {
		stop()
	}
	auditRetentionMu.Lock()
	stopAudit := auditRetentionStop
	auditRetentionStop = nil
	auditRetentionMu.Unlock()
	if stopAudit != nil {
		stopAudit()
	}
	usage.StopRedis()
}

type bootstrapAdminPassword struct {
	value  string
	source string
}

type identityBootstrapper interface {
	Bootstrap(context.Context, string) error
}

func resolveBootstrapAdminPassword(cfg *config.Config) bootstrapAdminPassword {
	if value := strings.TrimSpace(os.Getenv("CLIRELAY_ADMIN_PASSWORD")); value != "" {
		return bootstrapAdminPassword{value: value, source: "CLIRELAY_ADMIN_PASSWORD"}
	}
	if value := strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD")); value != "" {
		return bootstrapAdminPassword{value: value, source: "MANAGEMENT_PASSWORD"}
	}
	return bootstrapAdminPassword{
		value:  strings.TrimSpace(cfg.RemoteManagement.SecretKey),
		source: "remote-management.secret-key",
	}
}

func bootstrapIdentity(ctx context.Context, service identityBootstrapper, password bootstrapAdminPassword) error {
	if err := service.Bootstrap(ctx, password.value); err != nil {
		return fmt.Errorf("identity bootstrap using admin password from %s: %w", password.source, err)
	}
	return nil
}

func initializeRuntimeDataStack(cfg *config.Config, configPath string, loc *time.Location) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if err := usage.InitPostgres(cfg.Postgres, cfg.RequestLogStorage, loc); err != nil {
		return err
	}
	bootstrapPassword := resolveBootstrapAdminPassword(cfg)
	identityService := identity.NewService(usage.RuntimeDB())
	enduserService := enduser.NewService(usage.RuntimeDB())
	enduser.SetDefault(enduserService)
	if err := bootstrapIdentity(context.Background(), identityService, bootstrapPassword); err != nil {
		return err
	}
	identity.SetDefault(identityService)
	identityService.SetSessionPolicy(identity.SessionPolicyFromConfig(cfg))
	startSessionReaper(identityService, identity.SessionReaperIntervalFromConfig(cfg))
	startAuditRetention(identityService, identity.AuditRetentionPolicyFromConfig(cfg))
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
	middleware.InitQuotaEndUserUsageFuncs(usage.CountTodayByEndUser, usage.CountTotalByEndUser, usage.QueryTotalCostByEndUser, usage.QueryTodayCostByEndUser)
	middleware.InitQuotaPeriodUsageFuncs(usage.QueryPeriodSpendingByAPIKeyIDForTenant, usage.QueryPeriodSpendingByEndUserForTenant)
	usage.SetTokenUsageCallback(func(apiKey string, totalTokens int64) {
		endUserID := ""
		if row := usage.GetAPIKey(apiKey); row != nil {
			endUserID = row.EndUserID
		}
		middleware.RecordTokenUsageForRequest(apiKey, endUserID, totalTokens)
	})
	return nil
}

type runtimeDataStackMaintenanceOps struct {
	runAIAccountSharedSubjectBackfill func() error
	scheduleUsageRollupCatchup        func()
}

func defaultRuntimeDataStackMaintenanceOps() runtimeDataStackMaintenanceOps {
	return runtimeDataStackMaintenanceOps{
		runAIAccountSharedSubjectBackfill: func() error {
			if _, err := usage.RunAIAccountSharedSubjectBackfillAtInit(); err != nil {
				return err
			}
			return usage.RunAIAccountSubjectUsageTokensBackfillAtInit()
		},
		scheduleUsageRollupCatchup: usage.ScheduleUsageRollupBlueGreenCatchup,
	}
}

func runtimeDataStackPostStartHooks(ops runtimeDataStackMaintenanceOps) cliproxy.Hooks {
	return cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) {
		go runRuntimeDataStackPostStartMaintenance(ops)
	}}
}

func runRuntimeDataStackPostStartMaintenance(ops runtimeDataStackMaintenanceOps) {
	startedAt := time.Now()
	log.Info("usage: post-listen runtime data maintenance started")

	if ops.runAIAccountSharedSubjectBackfill != nil {
		stepStartedAt := time.Now()
		if err := ops.runAIAccountSharedSubjectBackfill(); err != nil {
			log.WithError(err).Error("usage: post-listen ai account shared subject backfill failed")
		} else {
			log.Infof("usage: post-listen ai account shared subject backfill completed in %s", time.Since(stepStartedAt).Round(time.Millisecond))
		}
	}
	// Old blue-green slot may keep writing request_logs until drain. Run the one
	// absolute rebuild only after the drain window so it includes both slots and
	// cannot block the HTTP listener or deployment readiness.
	if ops.scheduleUsageRollupCatchup != nil {
		ops.scheduleUsageRollupCatchup()
	}

	log.Infof("usage: post-listen runtime data maintenance scheduled in %s", time.Since(startedAt).Round(time.Millisecond))
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

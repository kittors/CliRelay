package api

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	internalserviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func (s *Server) buildDefaultConfigMutatedHook(initial *config.Config, configFilePath string) func(*config.Config) {
	return func(updated *config.Config) {
		if updated == nil {
			updated = initial
		}
		if updated == nil {
			return
		}
		applyStoredConfigOverlays(updated, configFilePath)
		s.UpdateClients(updated)
	}
}

func applyStoredConfigOverlays(cfg *config.Config, configFilePath string) {
	internalserviceapp.ApplyDBBackedRuntimeSettings(cfg, configFilePath)
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
func (s *Server) UpdateClients(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}

	oldCfg := s.oldConfigSnapshot()
	s.applyRequestLoggerConfig(oldCfg, cfg)
	s.applyProcessLoggingConfig(oldCfg, cfg)
	s.applyUsageStatisticsConfig(oldCfg, cfg)
	usage.SetRequestLogBodyStorageEnabled(cfg.RequestLogStorage.StoreContent)
	s.applyAuthRuntimeConfig(oldCfg, cfg)
	s.applyRequestBodyConfig(oldCfg, cfg)
	s.applyRuntimeLogLevel(oldCfg, cfg)
	s.applyProxyWarmupConfig(cfg)
	s.updateManagementRouteAvailability(oldCfg, cfg)
	s.applyAccessConfig(oldCfg, cfg)
	s.commitUpdatedConfig(oldCfg, cfg)
	s.syncConfigDerivedAuths(cfg)
	s.refreshServerHandlers(cfg)
	s.refreshAmpModule(oldCfg, cfg)
	s.syncTokenStoreBaseDir(cfg)
	s.logClientUpdateSummary(cfg)
}

func (s *Server) applyRequestBodyConfig(oldCfg, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if oldCfg == nil || oldCfg.ModelRequestBodyLimitBytes() != cfg.ModelRequestBodyLimitBytes() {
		bodyutil.SetModelRequestBodyLimit(cfg.ModelRequestBodyLimitBytes())
	}
	if oldCfg == nil || oldCfg.RequestBodyDiskThresholdBytes() != cfg.RequestBodyDiskThresholdBytes() {
		bodyutil.SetRequestBodyDiskThreshold(cfg.RequestBodyDiskThresholdBytes())
	}
	if oldCfg == nil || oldCfg.RequestBodyCacheDir() != cfg.RequestBodyCacheDir() {
		if cfg.RequestBodyCacheDir() == "" {
			bodyutil.ResetRequestBodyCacheDir()
		} else {
			bodyutil.SetRequestBodyCacheDir(cfg.RequestBodyCacheDir())
		}
		if err := bodyutil.CleanupOldRequestBodyCacheFiles(5 * time.Minute); err != nil {
			log.Warnf("failed to cleanup request body cache files: %v", err)
		}
	}
}

func (s *Server) oldConfigSnapshot() *config.Config {
	if s == nil || len(s.oldConfigYaml) == 0 {
		return nil
	}
	var oldCfg *config.Config
	_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	return oldCfg
}

func (s *Server) applyRequestLoggerConfig(oldCfg, cfg *config.Config) {
	if s == nil || cfg == nil || s.requestLogger == nil {
		return
	}
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if oldCfg == nil || previousRequestLog != cfg.RequestLog {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}
	if oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}
}

func (s *Server) applyProcessLoggingConfig(oldCfg, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}
}

func (s *Server) applyUsageStatisticsConfig(oldCfg, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}
}

func (s *Server) applyAuthRuntimeConfig(oldCfg, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}
	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second)
	}
}

func (s *Server) applyRuntimeLogLevel(oldCfg, cfg *config.Config) {
	if cfg == nil {
		return
	}
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}
}

func (s *Server) updateManagementRouteAvailability(oldCfg, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
		return
	}

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
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

func (s *Server) commitUpdatedConfig(oldCfg, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.cfg = cfg
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	managementasset.SetCurrentConfig(cfg)
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
}

func (s *Server) refreshServerHandlers(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	if s.handlers != nil {
		s.handlers.UpdateClients(&cfg.SDKConfig)
	}
	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		if s.handlers != nil {
			s.mgmt.SetAuthManager(s.handlers.AuthManager)
		}
	}
}

func (s *Server) refreshAmpModule(oldCfg, cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	ampConfigChanged := oldCfg == nil || !reflect.DeepEqual(oldCfg.AmpCode, cfg.AmpCode)
	if !ampConfigChanged {
		return
	}
	if s.ampModule != nil {
		log.Debugf("triggering amp module config update")
		if err := s.ampModule.OnConfigUpdated(cfg); err != nil {
			log.Errorf("failed to update Amp module config: %v", err)
		}
		return
	}
	log.Warnf("amp module is nil, skipping config update")
}

func (s *Server) syncTokenStoreBaseDir(cfg *config.Config) {
	if cfg == nil {
		return
	}
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
}

func (s *Server) logClientUpdateSummary(cfg *config.Config) {
	if cfg == nil {
		return
	}
	tokenStore := sdkAuth.GetTokenStore()
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

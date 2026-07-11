package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// LoadConfig reads a YAML configuration file from the given path,
// unmarshals it into a Config struct, applies environment variable overrides,
// and returns it.
//
// Parameters:
//   - configFile: The path to the YAML configuration file
//
// Returns:
//   - *Config: The loaded configuration
//   - error: An error if the configuration could not be loaded
func LoadConfig(configFile string) (*Config, error) {
	return LoadConfigOptional(configFile, false)
}

// LoadConfigOptional reads YAML from configFile.
// If optional is true and the file is missing, it returns an empty Config.
// If optional is true and the file is empty or invalid, it returns an empty Config.
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	// NOTE: Startup oauth-model-alias migration is intentionally disabled.
	// Reason: avoid mutating config.yaml during server startup.
	// Re-enable the block below if automatic startup migration is needed again.
	// if migrated, err := MigrateOAuthModelAlias(configFile); err != nil {
	// 	// Log warning but don't fail - config loading should still work
	// 	fmt.Printf("Warning: oauth-model-alias migration failed: %v\n", err)
	// } else if migrated {
	// 	fmt.Println("Migrated oauth-model-mappings to oauth-model-alias")
	// }

	// Read the entire configuration file into memory.
	data, err := os.ReadFile(configFile)
	if err != nil {
		if optional {
			if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
				// Missing and optional: return empty config (cloud deploy standby).
				cfg := &Config{}
				cfg.SanitizeIdentityFingerprint()
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// In cloud deploy mode (optional=true), if file is empty or contains only whitespace, return empty config.
	if optional && len(data) == 0 {
		cfg := &Config{}
		cfg.SanitizeIdentityFingerprint()
		return cfg, nil
	}

	// Unmarshal the YAML data into the Config struct.
	var cfg Config
	// Set defaults before unmarshal so that absent keys keep defaults.
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.RequestBody.ModelMaxMB = DefaultModelRequestBodyMB
	cfg.RequestBody.DiskThresholdMB = DefaultRequestBodyDiskThresholdMB
	cfg.UsageStatisticsEnabled = false
	cfg.RequestLogStorage.StoreContent = false
	cfg.RequestLogStorage.ContentRetentionDays = 30
	cfg.RequestLogStorage.CleanupIntervalMinutes = 1440
	// Default cap for stored request/response bodies in usage.db.
	// This controls the compressed body payloads only (metadata rows are separate).
	cfg.RequestLogStorage.MaxTotalSizeMB = 1024
	cfg.RequestLogStorage.VacuumOnCleanup = true
	cfg.DisableCooling = false
	cfg.Routing.IncludeDefaultGroup = true
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.AmpCode.RestrictManagementToLocalhost = false // Default to false: API key auth is sufficient
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.AutoUpdate.Enabled = true
	cfg.AutoUpdate.Channel = DefaultAutoUpdateChannel
	cfg.AutoUpdate.Repository = DefaultAutoUpdateRepository
	cfg.AutoUpdate.DockerImage = DefaultAutoUpdateDockerImage
	cfg.AutoUpdate.UpdaterURL = DefaultAutoUpdateUpdaterURL
	cfg.ProxyWarmup = defaultProxyWarmConfig()
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		if optional {
			// In cloud deploy mode, if YAML parsing fails, return empty config instead of error.
			cfg := &Config{}
			cfg.SanitizeIdentityFingerprint()
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Detect legacy keys but intentionally do not migrate/persist on startup.
	// var legacy legacyConfigData
	// if err = yaml.Unmarshal(data, &legacy); err == nil {
	// 	if cfg.migrateLegacyGeminiKeys(legacy.LegacyGeminiKeys) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// 	if cfg.migrateLegacyOpenAICompatibilityKeys(legacy.OpenAICompat) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// 	if cfg.migrateLegacyAmpConfig(&legacy) {
	// 		cfg.legacyMigrationPending = true
	// 	}
	// }

	cfg.ApplyEnvOverrides()
	cfg.SanitizeIdentityFingerprint()
	cfg.SanitizeCodexOAuthAdmission()
	cfg.SanitizeOAuthModelAlias()
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)
	cfg.SanitizeAutoUpdate()
	cfg.SanitizeOpenAICompatibility()
	cfg.SanitizeCodexKeys()
	cfg.SanitizeClaudeKeys()
	cfg.SanitizeOpenCodeGoKeys()
	cfg.SanitizeClineKeys()
	cfg.SanitizeOllamaCloudKeys()
	cfg.SanitizeGeminiKeys()
	cfg.SanitizeProxyWarmup()

	// Normalize secret-key: if provided and not bcrypt-hashed, hash it on load for runtime use.
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		if hashed, errHash := hashSecret(cfg.RemoteManagement.SecretKey); errHash == nil {
			cfg.RemoteManagement.SecretKey = hashed
		}
	}

	// Fill derived defaults after YAML if fields were omitted.
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}
	if cfg.RequestRetry < 0 {
		cfg.RequestRetry = 0
	}
	if cfg.MaxRetryInterval < 0 {
		cfg.MaxRetryInterval = 0
	}
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		cfg.AuthDir = "auth"
	}
	if strings.TrimSpace(cfg.Host) == "localhost" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 8315
	}
	if cfg.Postgres.MaxOpenConns <= 0 {
		cfg.Postgres.MaxOpenConns = 16
	}
	if cfg.Postgres.MaxIdleConns <= 0 {
		cfg.Postgres.MaxIdleConns = 4
	}
	if cfg.RequestBody.ModelMaxMB <= 0 {
		cfg.RequestBody.ModelMaxMB = DefaultModelRequestBodyMB
	}
	if cfg.RequestBody.DiskThresholdMB <= 0 {
		cfg.RequestBody.DiskThresholdMB = DefaultRequestBodyDiskThresholdMB
	}
	if cfg.RequestLogStorage.ContentRetentionDays < 0 {
		cfg.RequestLogStorage.ContentRetentionDays = 0
	}
	if cfg.RequestLogStorage.CleanupIntervalMinutes <= 0 {
		cfg.RequestLogStorage.CleanupIntervalMinutes = 1440
	}
	if cfg.RequestLogStorage.MaxTotalSizeMB < 0 {
		cfg.RequestLogStorage.MaxTotalSizeMB = 0
	}
	if cfg.RequestLogStorage.ContentRetentionDays == 0 && !cfg.RequestLogStorage.StoreContent {
		cfg.RequestLogStorage.StoreContent = false
	}
	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 0
	}
	if cfg.Streaming.KeepAliveSeconds == 0 {
		cfg.Streaming.KeepAliveSeconds = 15
	}
	if cfg.AutoUpdate.Channel == "" {
		cfg.AutoUpdate.Channel = DefaultAutoUpdateChannel
	}
	if strings.TrimSpace(cfg.AutoUpdate.Channel) == "" {
		cfg.AutoUpdate.Channel = DefaultAutoUpdateChannel
	}
	if strings.TrimSpace(cfg.AutoUpdate.Repository) == "" {
		cfg.AutoUpdate.Repository = DefaultAutoUpdateRepository
	}
	if strings.TrimSpace(cfg.AutoUpdate.DockerImage) == "" {
		cfg.AutoUpdate.DockerImage = DefaultAutoUpdateDockerImage
	}
	if strings.TrimSpace(cfg.AutoUpdate.UpdaterURL) == "" {
		cfg.AutoUpdate.UpdaterURL = DefaultAutoUpdateUpdaterURL
	}
	if cfg.Routing.Strategy == "" {
		cfg.Routing.Strategy = "round-robin"
	}
	// Validate raw payload rules and drop invalid entries.
	cfg.SanitizePayloadRules()

	// NOTE: Legacy migration persistence is intentionally disabled together with
	// startup legacy migration to keep startup read-only for config.yaml.
	// Re-enable the block below if automatic startup migration is needed again.
	// if cfg.legacyMigrationPending {
	// 	fmt.Println("Detected legacy configuration keys, attempting to persist the normalized config...")
	// 	if !optional && configFile != "" {
	// 		if err := SaveConfigPreserveComments(configFile, &cfg); err != nil {
	// 			return nil, fmt.Errorf("failed to persist migrated legacy config: %w", err)
	// 		}
	// 		fmt.Println("Legacy configuration normalized and persisted.")
	// 	} else {
	// 		fmt.Println("Legacy configuration normalized in memory; persistence skipped.")
	// 	}
	// }

	// Return the populated configuration struct.
	return &cfg, nil
}

// ApplyEnvOverrides applies process-level configuration overrides.
func (cfg *Config) ApplyEnvOverrides() {
	if cfg == nil {
		return
	}
	if authPath := strings.TrimSpace(os.Getenv(EnvAuthPath)); authPath != "" {
		cfg.AuthDir = authPath
	}
	if dsn := strings.TrimSpace(os.Getenv(EnvPostgresDSN)); dsn != "" {
		cfg.Postgres.DSN = dsn
	}
	if raw := strings.TrimSpace(os.Getenv(EnvRedisEnable)); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			cfg.Redis.Enable = enabled
		}
	}
	if addr := strings.TrimSpace(os.Getenv(EnvRedisAddr)); addr != "" {
		cfg.Redis.Addr = addr
	}
	if password := os.Getenv(EnvRedisPassword); password != "" {
		cfg.Redis.Password = password
	}
	if raw := strings.TrimSpace(os.Getenv(EnvRedisDB)); raw != "" {
		if db, err := strconv.Atoi(raw); err == nil && db >= 0 {
			cfg.Redis.DB = db
		}
	}
	for _, key := range []string{EnvPort, EnvLegacyPort} {
		if rawPort := strings.TrimSpace(os.Getenv(key)); rawPort != "" {
			if port, err := strconv.Atoi(rawPort); err == nil && port > 0 {
				cfg.Port = port
			}
			return
		}
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaultsDisableControlPanel(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.RemoteManagement.DisableControlPanel {
		t.Fatalf("DisableControlPanel = true, want false by default")
	}
	if cfg.MainAPIReadTimeout() != DefaultMainAPIReadTimeout {
		t.Fatalf("MainAPIReadTimeout = %s, want %s", cfg.MainAPIReadTimeout(), DefaultMainAPIReadTimeout)
	}
	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("PanelGitHubRepository = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
	}
}

func TestLoadConfigDefaultsRequestLogBodyStorageDisabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.RequestLogStorage.StoreContent {
		t.Fatal("RequestLogStorage.StoreContent = true, want false by default")
	}
}

func TestLoadConfigReadsMainAPIReadTimeoutOverride(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("main-api-read-timeout-seconds: 240\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.MainAPIReadTimeoutSeconds != 240 {
		t.Fatalf("MainAPIReadTimeoutSeconds = %d, want 240", cfg.MainAPIReadTimeoutSeconds)
	}
	if cfg.MainAPIReadTimeout() != 240*time.Second {
		t.Fatalf("MainAPIReadTimeout = %s, want %s", cfg.MainAPIReadTimeout(), 240*time.Second)
	}
}

func TestLoadConfigReadsRequestBodyModelLimit(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("request-body:\n  model-max-mb: 64\n  disk-threshold-mb: 4\n  cache-dir: /tmp/clirelay-body-cache\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.RequestBody.ModelMaxMB != 64 {
		t.Fatalf("RequestBody.ModelMaxMB = %d, want 64", cfg.RequestBody.ModelMaxMB)
	}
	if cfg.ModelRequestBodyLimitBytes() != 64<<20 {
		t.Fatalf("ModelRequestBodyLimitBytes = %d, want %d", cfg.ModelRequestBodyLimitBytes(), int64(64<<20))
	}
	if cfg.RequestBody.DiskThresholdMB != 4 {
		t.Fatalf("RequestBody.DiskThresholdMB = %d, want 4", cfg.RequestBody.DiskThresholdMB)
	}
	if cfg.RequestBodyDiskThresholdBytes() != 4<<20 {
		t.Fatalf("RequestBodyDiskThresholdBytes = %d, want %d", cfg.RequestBodyDiskThresholdBytes(), int64(4<<20))
	}
	if cfg.RequestBodyCacheDir() != "/tmp/clirelay-body-cache" {
		t.Fatalf("RequestBodyCacheDir = %q, want /tmp/clirelay-body-cache", cfg.RequestBodyCacheDir())
	}
}

func TestLoadConfigDefaultsRequestBodyModelLimit(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.RequestBody.ModelMaxMB != DefaultModelRequestBodyMB {
		t.Fatalf("RequestBody.ModelMaxMB = %d, want %d", cfg.RequestBody.ModelMaxMB, DefaultModelRequestBodyMB)
	}
	if cfg.RequestBody.DiskThresholdMB != DefaultRequestBodyDiskThresholdMB {
		t.Fatalf("RequestBody.DiskThresholdMB = %d, want %d", cfg.RequestBody.DiskThresholdMB, DefaultRequestBodyDiskThresholdMB)
	}
}

func TestLoadConfigSanitizesProxyWarmupDefaults(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("proxy-warmup:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if !cfg.ProxyWarmup.Enabled {
		t.Fatal("ProxyWarmup.Enabled = false, want true")
	}
	if cfg.ProxyWarmup.IntervalSeconds <= 0 {
		t.Fatalf("ProxyWarmup.IntervalSeconds = %d, want default > 0", cfg.ProxyWarmup.IntervalSeconds)
	}
	if len(cfg.ProxyWarmup.Targets) == 0 {
		t.Fatal("ProxyWarmup.Targets is empty, want default warm targets")
	}
	if cfg.ProxyWarmup.Targets[0].Method == "" {
		t.Fatal("ProxyWarmup target method is empty, want sanitized default")
	}
}

func TestSanitizeRoutingPreservesChannelGroupSettings(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Routing: RoutingConfig{
			Strategy: "sticky",
			ChannelGroups: []RoutingChannelGroup{
				{
					Name:               " Team ",
					Strategy:           "sessionsticky",
					ExcludeFromDefault: true,
					Match: ChannelGroupMatch{
						Channels: []string{"Team Channel"},
					},
				},
				{
					Name:               " Default ",
					Strategy:           "ff",
					ExcludeFromDefault: true,
					Match: ChannelGroupMatch{
						Channels: []string{"Cache Channel"},
					},
				},
			},
		},
	}

	cfg.SanitizeRouting()

	if got := cfg.Routing.Strategy; got != "session-sticky" {
		t.Fatalf("global strategy = %q, want session-sticky", got)
	}
	if got := cfg.Routing.ChannelGroups[0].Strategy; got != "session-sticky" {
		t.Fatalf("group strategy = %q, want session-sticky", got)
	}
	if got := cfg.Routing.ChannelGroups[1].Strategy; got != "fill-first" {
		t.Fatalf("group strategy alias = %q, want fill-first", got)
	}
	if !cfg.Routing.ChannelGroups[0].ExcludeFromDefault {
		t.Fatal("exclude-from-default should be preserved for non-default groups")
	}
	if cfg.Routing.ChannelGroups[1].ExcludeFromDefault {
		t.Fatal("exclude-from-default should be cleared for the default group")
	}
}

func TestLoadConfigAllowsAuthPathEnvOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("auth-dir: /root/.cli-proxy-api\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AUTH_PATH", "/CLIProxyAPI/auths")

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.AuthDir != "/CLIProxyAPI/auths" {
		t.Fatalf("AuthDir = %q, want AUTH_PATH override", cfg.AuthDir)
	}
}

func TestLoadConfigAllowsPortEnvOverride(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8318\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvPort, "8319")

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Port != 8319 {
		t.Fatalf("Port = %d, want CLIRELAY_PORT override", cfg.Port)
	}
}

func TestLoadConfigAllowsDataStackEnvOverrides(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
postgres:
  dsn: postgres://old/cliproxy
redis:
  enable: false
  addr: 127.0.0.1:6379
  password: old
  db: 0
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvPostgresDSN, "postgres://cliproxy:cliproxy@postgres:5432/cliproxy?sslmode=disable")
	t.Setenv(EnvRedisEnable, "true")
	t.Setenv(EnvRedisAddr, "redis:6379")
	t.Setenv(EnvRedisPassword, "secret")
	t.Setenv(EnvRedisDB, "2")

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Postgres.DSN != "postgres://cliproxy:cliproxy@postgres:5432/cliproxy?sslmode=disable" {
		t.Fatalf("Postgres.DSN = %q", cfg.Postgres.DSN)
	}
	if !cfg.Redis.Enable || cfg.Redis.Addr != "redis:6379" || cfg.Redis.Password != "secret" || cfg.Redis.DB != 2 {
		t.Fatalf("Redis override = %+v", cfg.Redis)
	}
}

func TestLoadConfigDefaultsAutoUpdateEnabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if !cfg.AutoUpdate.Enabled {
		t.Fatalf("AutoUpdate.Enabled = false, want true by default")
	}
	if cfg.AutoUpdate.Channel != "main" {
		t.Fatalf("AutoUpdate.Channel = %q, want main", cfg.AutoUpdate.Channel)
	}
	if cfg.AutoUpdate.Repository != DefaultAutoUpdateRepository {
		t.Fatalf("AutoUpdate.Repository = %q, want %q", cfg.AutoUpdate.Repository, DefaultAutoUpdateRepository)
	}
	if cfg.AutoUpdate.DockerImage != DefaultAutoUpdateDockerImage {
		t.Fatalf("AutoUpdate.DockerImage = %q, want %q", cfg.AutoUpdate.DockerImage, DefaultAutoUpdateDockerImage)
	}
	if cfg.AutoUpdate.UpdaterURL != DefaultAutoUpdateUpdaterURL {
		t.Fatalf("AutoUpdate.UpdaterURL = %q, want %q", cfg.AutoUpdate.UpdaterURL, DefaultAutoUpdateUpdaterURL)
	}
	if !cfg.IdentityFingerprint.Codex.Enabled ||
		!cfg.IdentityFingerprint.Claude.Enabled ||
		!cfg.IdentityFingerprint.Gemini.Enabled ||
		!cfg.IdentityFingerprint.XAI.Enabled {
		t.Fatalf("IdentityFingerprint = %#v, want all providers enabled by default", cfg.IdentityFingerprint)
	}
}

func TestLoadConfigReadsDisabledAutoUpdate(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`port: 8317
auto-update:
  enabled: false
  channel: dev
  repository: kittors/CliRelay
  docker-image: ghcr.io/example/custom
  updater-url: http://updater.local:8320
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.AutoUpdate.Enabled {
		t.Fatalf("AutoUpdate.Enabled = true, want false from config")
	}
	if cfg.AutoUpdate.Channel != "dev" {
		t.Fatalf("AutoUpdate.Channel = %q, want dev", cfg.AutoUpdate.Channel)
	}
	if cfg.AutoUpdate.Repository != "kittors/CliRelay" {
		t.Fatalf("AutoUpdate.Repository = %q, want kittors/CliRelay", cfg.AutoUpdate.Repository)
	}
	if cfg.AutoUpdate.DockerImage != "ghcr.io/example/custom" {
		t.Fatalf("AutoUpdate.DockerImage = %q, want ghcr.io/example/custom", cfg.AutoUpdate.DockerImage)
	}
	if cfg.AutoUpdate.UpdaterURL != "http://updater.local:8320" {
		t.Fatalf("AutoUpdate.UpdaterURL = %q, want http://updater.local:8320", cfg.AutoUpdate.UpdaterURL)
	}
}

func TestLoadConfigIdentityFingerprintDefaultsAndExplicitDisabled(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte(`port: 8317
identity-fingerprint:
  xai:
    enabled: false
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if !cfg.IdentityFingerprint.Codex.Enabled ||
		!cfg.IdentityFingerprint.Claude.Enabled ||
		!cfg.IdentityFingerprint.Gemini.Enabled {
		t.Fatalf("IdentityFingerprint = %#v, want omitted providers enabled by default", cfg.IdentityFingerprint)
	}
	if cfg.IdentityFingerprint.XAI.Enabled {
		t.Fatalf("XAI.Enabled = true, want explicit false from config")
	}
}

func TestSaveConfigPreserveCommentsOmitsDisableControlPanelWhenDefaultFalse(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{
		Port: 8317,
		RemoteManagement: RemoteManagement{
			DisableControlPanel:   false,
			PanelGitHubRepository: DefaultPanelGitHubRepository,
		},
	}

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments returned error: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	rendered := string(data)
	if strings.Contains(rendered, "disable-control-panel:") {
		t.Fatalf("saved config unexpectedly persisted default disable-control-panel=false:\n%s", rendered)
	}
	if strings.Contains(rendered, "panel-github-repository:") {
		t.Fatalf("saved config unexpectedly persisted default panel repository:\n%s", rendered)
	}
}

func TestSaveConfigPreserveCommentsKeepsDisableControlPanelTrue(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{
		Port: 8317,
		RemoteManagement: RemoteManagement{
			DisableControlPanel:   true,
			PanelGitHubRepository: DefaultPanelGitHubRepository,
		},
	}

	if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments returned error: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	rendered := string(data)
	if !strings.Contains(rendered, "disable-control-panel: true") {
		t.Fatalf("saved config missing explicit true override:\n%s", rendered)
	}
}

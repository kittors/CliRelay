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

func TestSanitizeRoutingPreservesChannelGroupSettings(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Routing: RoutingConfig{
			Strategy: "fill-first",
			ChannelGroups: []RoutingChannelGroup{
				{
					Name:               " Team ",
					Strategy:           "round-robin",
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

	if got := cfg.Routing.ChannelGroups[0].Strategy; got != "round-robin" {
		t.Fatalf("group strategy = %q, want round-robin", got)
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

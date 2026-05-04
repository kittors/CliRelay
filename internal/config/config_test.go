package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("PanelGitHubRepository = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
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

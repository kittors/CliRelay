package main

import (
	"os"
	"strings"
	"testing"
)

// These are configuration drift guard tests: they assert generated install
// script text, not runtime behavior.
func TestInstallEnvProvidesHostAbsoluteBindPaths(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"CLI_PROXY_CONFIG_PATH=${INSTALL_DIR}/config.yaml",
		"CLI_PROXY_AUTH_PATH=${INSTALL_DIR}/auths",
		"AUTH_PATH=/root/.cli-proxy-api",
		"CLI_PROXY_LOG_PATH=${INSTALL_DIR}/logs",
		"CLI_PROXY_DATA_PATH=${INSTALL_DIR}/data",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
}

func TestInstallComposeUsesHostPathVariablesForDataMounts(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"${CLI_PROXY_CONFIG_PATH}:/CLIProxyAPI/config.yaml",
		"${CLI_PROXY_AUTH_PATH}:${AUTH_PATH}",
		"${CLI_PROXY_LOG_PATH}:/CLIProxyAPI/logs",
		"${CLI_PROXY_DATA_PATH}:/CLIProxyAPI/data",
		"AUTH_PATH: ${AUTH_PATH}",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("install.sh generated compose missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"./config.yaml:/CLIProxyAPI/config.yaml",
		"./auths:/root/.cli-proxy-api",
		"./logs:/CLIProxyAPI/logs",
		"./data:/CLIProxyAPI/data",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("install.sh generated compose still contains relative bind mount %q", forbidden)
		}
	}
}

func TestInstallComposeMirrorsDeploymentFilesAtHostPathInUpdater(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	content := string(data)
	updaterStart := strings.Index(content, "  clirelay-updater:\n")
	updaterEnd := strings.Index(content, "\n  postgres:")
	if updaterStart < 0 || updaterEnd <= updaterStart {
		t.Fatal("install.sh generated compose missing clirelay-updater service block")
	}
	updater := content[updaterStart:updaterEnd]

	for _, want := range []string{
		"CLIRELAY_COMPOSE_FILE: ${CLIRELAY_INSTALL_DIR}/docker-compose.yml",
		"CLIRELAY_ENV_FILE: ${CLIRELAY_INSTALL_DIR}/.env",
		"CLIRELAY_UPDATER_STATE_FILE: ${CLIRELAY_INSTALL_DIR}/.clirelay-updater-status.json",
		".:${CLIRELAY_INSTALL_DIR}",
		"healthcheck:\n      disable: true",
	} {
		if !strings.Contains(updater, want) {
			t.Fatalf("install.sh generated updater compose missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"CLIRELAY_COMPOSE_FILE: /workspace/docker-compose.yml",
		"CLIRELAY_ENV_FILE: /workspace/.env",
		"./docker-compose.yml:/workspace/docker-compose.yml:ro",
		"./.env:/workspace/.env",
		"./docker-compose.yml:${CLIRELAY_INSTALL_DIR}/docker-compose.yml:ro",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("install.sh generated updater compose still contains /workspace mapping %q", forbidden)
		}
	}
}

func TestInstallComposeIncludesRuntimeDataStack(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		`updater_token="${CLIRELAY_UPDATER_TOKEN:-$(rand_hex 16)}"`,
		`postgres_db="${CLIRELAY_POSTGRES_DB:-cliproxy}"`,
		`postgres_user="${CLIRELAY_POSTGRES_USER:-cliproxy}"`,
		`postgres_password="${CLIRELAY_POSTGRES_PASSWORD:-$(rand_hex 16)}"`,
		`postgres_dsn="${CLIRELAY_POSTGRES_DSN:-postgres://${postgres_user}:${postgres_password}@postgres:5432/${postgres_db}?sslmode=disable}"`,
		`redis_addr="${CLIRELAY_REDIS_ADDR:-redis:6379}"`,
		`redis_db="${CLIRELAY_REDIS_DB:-0}"`,
		"CLIRELAY_UPDATER_TOKEN=${updater_token}",
		"CLIRELAY_POSTGRES_DB=${postgres_db}",
		"CLIRELAY_POSTGRES_USER=${postgres_user}",
		"CLIRELAY_POSTGRES_DSN=${postgres_dsn}",
		"CLIRELAY_REDIS_ENABLE=true",
		"CLIRELAY_REDIS_ADDR=${redis_addr}",
		"CLIRELAY_REDIS_DB=${redis_db}",
		"CLIRELAY_POSTGRES_DATA_PATH=${postgres_data_path}",
		"CLIRELAY_REDIS_DATA_PATH=${redis_data_path}",
		"CLIRELAY_POSTGRES_DSN: ${CLIRELAY_POSTGRES_DSN}",
		"CLIRELAY_REDIS_ENABLE: ${CLIRELAY_REDIS_ENABLE}",
		"postgres:\n    image: postgres:15-alpine",
		"redis:\n    image: redis:7-alpine",
		"depends_on:\n      postgres:\n        condition: service_healthy\n      redis:\n        condition: service_healthy",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("install.sh missing runtime data stack text %q", want)
		}
	}
}

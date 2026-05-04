package main

import (
	"os"
	"strings"
	"testing"
)

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

	for _, want := range []string{
		"CLIRELAY_COMPOSE_FILE: ${CLIRELAY_INSTALL_DIR}/docker-compose.yml",
		"CLIRELAY_ENV_FILE: ${CLIRELAY_INSTALL_DIR}/.env",
		"./docker-compose.yml:${CLIRELAY_INSTALL_DIR}/docker-compose.yml:ro",
		"./.env:${CLIRELAY_INSTALL_DIR}/.env",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("install.sh generated updater compose missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"CLIRELAY_COMPOSE_FILE: /workspace/docker-compose.yml",
		"CLIRELAY_ENV_FILE: /workspace/.env",
		"./docker-compose.yml:/workspace/docker-compose.yml:ro",
		"./.env:/workspace/.env",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("install.sh generated updater compose still contains /workspace mapping %q", forbidden)
		}
	}
}

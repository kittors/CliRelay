package main

import (
	"os"
	"strings"
	"testing"
)

// These are configuration drift guard tests: they assert shipped compose text,
// not runtime behavior.
func TestRepositoryComposeUsesProjectDirForDefaultDataMounts(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"${CLI_PROXY_CONFIG_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/config.yaml}:/CLIProxyAPI/config.yaml",
		"${CLI_PROXY_AUTH_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/auths}:${AUTH_PATH:-/CLIProxyAPI/auths}",
		"${CLI_PROXY_LOG_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/logs}:/CLIProxyAPI/logs",
		"${CLI_PROXY_DATA_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/data}:/CLIProxyAPI/data",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("docker-compose.yml missing %q", want)
		}
	}
}

func TestRepositoryComposePassesContainerAuthPath(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	want := "AUTH_PATH: ${AUTH_PATH:-/CLIProxyAPI/auths}"
	if !strings.Contains(content, want) {
		t.Fatalf("docker-compose.yml missing %q", want)
	}
}

func TestRepositoryComposeGeneratesUpdaterTokenThroughInit(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"clirelay-init:",
		"command: [\"clirelay-init-env\"]",
		"CLIRELAY_ENV_FILE: /clirelay-deploy/.env",
		"condition: service_completed_successfully",
		". /clirelay-deploy/.env",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("docker-compose.yml missing init env text %q", want)
		}
	}
	for _, forbidden := range []string{
		"CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:-clirelay-local-updater}",
		"CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:?",
		"POSTGRES_PASSWORD: ${CLIRELAY_POSTGRES_PASSWORD:-cliproxy}",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("docker-compose.yml still contains generated secret fallback %q", forbidden)
		}
	}
}

func TestRepositoryComposeMirrorsDeploymentFilesAtProjectDirInUpdater(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)
	updaterStart := strings.Index(content, "  clirelay-updater:\n")
	updaterEnd := strings.Index(content, "\n  postgres:")
	if updaterStart < 0 || updaterEnd <= updaterStart {
		t.Fatal("docker-compose.yml missing clirelay-updater service block")
	}
	updater := content[updaterStart:updaterEnd]

	for _, want := range []string{
		"CLIRELAY_PROJECT_DIR: ${CLIRELAY_PROJECT_DIR:-${PWD:-.}}",
		"CLIRELAY_COMPOSE_FILE: ${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/docker-compose.yml",
		"CLIRELAY_ENV_FILE: ${CLIRELAY_ENV_FILE:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/.env}",
		"CLIRELAY_UPDATER_STATE_FILE: ${CLIRELAY_UPDATER_STATE_FILE:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/.clirelay-updater-status.json}",
		"${CLIRELAY_PROJECT_DIR:-${PWD:-.}}:${CLIRELAY_PROJECT_DIR:-${PWD:-.}}",
		"${CLI_PROXY_CONFIG_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/config.yaml}:/CLIProxyAPI/config.yaml",
		"healthcheck:\n      disable: true",
	} {
		if !strings.Contains(updater, want) {
			t.Fatalf("docker-compose.yml updater config missing %q", want)
		}
	}

	for _, forbidden := range []string{
		"/workspace/docker-compose.yml",
		"/workspace/.env",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("docker-compose.yml still contains updater /workspace path %q", forbidden)
		}
	}
}

func TestRepositoryComposeDoesNotCreateEnvDirectoryForUpdater(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	for _, forbidden := range []string{
		"./.env:${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/.env",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("docker-compose.yml should not default updater to missing .env bind %q", forbidden)
		}
	}
}

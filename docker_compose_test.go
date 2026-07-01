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

func TestRepositoryComposeRequiresUpdaterToken(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	want := "CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:?CLIRELAY_UPDATER_TOKEN is required for updater sidecar}"
	if got := strings.Count(content, want); got != 2 {
		t.Fatalf("docker-compose.yml has %d required updater token entries, want 2", got)
	}
	if strings.Contains(content, "CLIRELAY_UPDATER_TOKEN: ${CLIRELAY_UPDATER_TOKEN:-}") {
		t.Fatal("docker-compose.yml still allows an empty updater token")
	}
}

func TestRepositoryComposeMirrorsDeploymentFilesAtProjectDirInUpdater(t *testing.T) {
	data, err := os.ReadFile("docker-compose.yml")
	if err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"CLIRELAY_PROJECT_DIR: ${CLIRELAY_PROJECT_DIR:-${PWD:-.}}",
		"CLIRELAY_COMPOSE_FILE: ${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/docker-compose.yml",
		"CLIRELAY_ENV_FILE: ${CLIRELAY_ENV_FILE:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/.env}",
		"${CLIRELAY_PROJECT_DIR:-${PWD:-.}}:${CLIRELAY_PROJECT_DIR:-${PWD:-.}}",
		"${CLI_PROXY_CONFIG_PATH:-${CLIRELAY_PROJECT_DIR:-${PWD:-.}}/config.yaml}:/CLIProxyAPI/config.yaml",
	} {
		if !strings.Contains(content, want) {
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

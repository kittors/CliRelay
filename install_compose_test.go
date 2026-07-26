package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
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

func TestInstallEnvGeneratesBootstrapAdminPassword(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	content := string(data)

	// Without a generated admin password the identity bootstrap falls back to the
	// empty remote-management.secret-key and a fresh install crash-loops.
	for _, want := range []string{
		`CFG_ADMIN_PASSWORD="$(resolve_admin_password)"`,
		"CLIRELAY_ADMIN_PASSWORD=${admin_password}",
		"CLIRELAY_ADMIN_PASSWORD: ${CLIRELAY_ADMIN_PASSWORD}",
		// The generated credential has to be reported, or the operator cannot sign in.
		"Panel login : admin /",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
}

// Executes the password generator defined in install.sh and checks the result against
// the same validation the bootstrap applies, so a hex-only generator cannot regress.
func TestInstallRandPasswordPassesIdentityValidation(t *testing.T) {
	script := `set -euo pipefail
` + extractShellFunctions(t, "install.sh", "rand_hex", "rand_password") + `
rand_password 16`

	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("run install.sh rand_password: %v", err)
	}
	password := strings.TrimSpace(string(out))
	if password == "" {
		t.Fatal("rand_password produced an empty value")
	}
	if _, err := identity.HashPassword(password); err != nil {
		t.Fatalf("install.sh rand_password produced %q, rejected by identity.HashPassword: %v", password, err)
	}
}

// extractShellFunctions pulls top-level `name() { ... }` blocks out of a shell script so
// a test can run them without executing the whole installer.
func extractShellFunctions(t *testing.T, path string, names ...string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	var out []string
	for _, name := range names {
		header := name + "() {"
		start := -1
		for i, line := range lines {
			if strings.TrimSpace(line) == header {
				start = i
				break
			}
		}
		if start < 0 {
			t.Fatalf("%s does not define %s()", path, name)
		}
		end := -1
		for i := start + 1; i < len(lines); i++ {
			if lines[i] == "}" {
				end = i
				break
			}
		}
		if end < 0 {
			t.Fatalf("%s: unterminated %s()", path, name)
		}
		out = append(out, strings.Join(lines[start:end+1], "\n"))
	}
	return strings.Join(out, "\n")
}

// The bootstrap admin password reuses the management secret when that secret satisfies
// the identity policy, so the operator keeps the one credential they were shown. This
// exercises the real shell functions rather than asserting on their source text.
func TestInstallResolveAdminPasswordPrefersCompliantManagementSecret(t *testing.T) {
	fns := extractShellFunctions(t, "install.sh", "rand_hex", "rand_password", "password_is_valid", "resolve_admin_password")

	compliant := runShell(t, fns+"\nCFG_SECRET='MyStr0ng!Secret'\nresolve_admin_password")
	if compliant != "MyStr0ng!Secret" {
		t.Fatalf("compliant secret was not reused: %q", compliant)
	}

	generated := runShell(t, fns+"\nCFG_SECRET='weak'\nresolve_admin_password")
	if generated == "weak" {
		t.Fatal("a secret that fails the password policy was reused as the admin password")
	}
	if _, err := identity.HashPassword(generated); err != nil {
		t.Fatalf("generated admin password %q is rejected: %v", generated, err)
	}
}

// The auto-generated management secret doubles as the admin password, so it too must
// satisfy the policy.
func TestInstallGeneratedManagementSecretSatisfiesPasswordPolicy(t *testing.T) {
	fns := extractShellFunctions(t, "install.sh", "rand_hex", "rand_password")
	secret := runShell(t, fns+"\nrand_password 16")
	if _, err := identity.HashPassword(secret); err != nil {
		t.Fatalf("generated management secret %q is rejected as an admin password: %v", secret, err)
	}
}

func runShell(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("bash", "-c", "set -euo pipefail\n"+script).Output()
	if err != nil {
		t.Fatalf("run shell snippet: %v", err)
	}
	return strings.TrimSpace(string(out))
}

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeInitEnvGeneratesMissingEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")

	cmd := exec.Command("sh", "scripts/init-compose-env.sh")
	cmd.Env = append(os.Environ(),
		"CLIRELAY_ENV_FILE="+envFile,
		"CLIRELAY_PROJECT_DIR="+dir,
		"CLI_PROXY_IMAGE=ghcr.io/kittors/clirelay:test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-compose-env failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	values := readEnvFile(t, envFile)
	for _, key := range []string{
		"CLI_PROXY_IMAGE",
		"CLIRELAY_PROJECT_DIR",
		"CLIRELAY_UPDATER_URL",
		"CLIRELAY_UPDATER_TOKEN",
		"CLIRELAY_ADMIN_PASSWORD",
		"CLIRELAY_POSTGRES_PASSWORD",
		"CLIRELAY_POSTGRES_DSN",
		"CLIRELAY_REDIS_ENABLE",
		"CLIRELAY_REDIS_ADDR",
	} {
		if values[key] == "" {
			t.Fatalf("%s was not generated in .env: %#v", key, values)
		}
	}
	if values["CLI_PROXY_IMAGE"] != "ghcr.io/kittors/clirelay:test" {
		t.Fatalf("CLI_PROXY_IMAGE = %q", values["CLI_PROXY_IMAGE"])
	}
	if len(values["CLIRELAY_UPDATER_TOKEN"]) != 32 {
		t.Fatalf("updater token length = %d, want 32", len(values["CLIRELAY_UPDATER_TOKEN"]))
	}
	if len(values["CLIRELAY_ADMIN_PASSWORD"]) != 32 {
		t.Fatalf("admin password length = %d, want 32", len(values["CLIRELAY_ADMIN_PASSWORD"]))
	}
	if len(values["CLIRELAY_POSTGRES_PASSWORD"]) != 32 {
		t.Fatalf("postgres password length = %d, want 32", len(values["CLIRELAY_POSTGRES_PASSWORD"]))
	}
	if !strings.Contains(values["CLIRELAY_POSTGRES_DSN"], values["CLIRELAY_POSTGRES_PASSWORD"]) {
		t.Fatalf("postgres DSN does not contain generated password")
	}
}

func TestComposeInitEnvPreservesExistingValues(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	if err := os.WriteFile(envFile, []byte("CLIRELAY_UPDATER_TOKEN=custom-token\nCLIRELAY_ADMIN_PASSWORD=custom-admin-password\nCLIRELAY_POSTGRES_PASSWORD=custom-pass\nCLIRELAY_POSTGRES_DB=customdb\n"), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}

	cmd := exec.Command("sh", "scripts/init-compose-env.sh")
	cmd.Env = append(os.Environ(),
		"CLIRELAY_ENV_FILE="+envFile,
		"CLIRELAY_PROJECT_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-compose-env failed: %v: %s", err, strings.TrimSpace(string(out)))
	}

	values := readEnvFile(t, envFile)
	if values["CLIRELAY_UPDATER_TOKEN"] != "custom-token" {
		t.Fatalf("updater token = %q, want custom-token", values["CLIRELAY_UPDATER_TOKEN"])
	}
	if values["CLIRELAY_ADMIN_PASSWORD"] != "custom-admin-password" {
		t.Fatalf("admin password = %q, want custom-admin-password", values["CLIRELAY_ADMIN_PASSWORD"])
	}
	if values["CLIRELAY_POSTGRES_PASSWORD"] != "custom-pass" {
		t.Fatalf("postgres password = %q, want custom-pass", values["CLIRELAY_POSTGRES_PASSWORD"])
	}
	if !strings.Contains(values["CLIRELAY_POSTGRES_DSN"], "customdb") || !strings.Contains(values["CLIRELAY_POSTGRES_DSN"], "custom-pass") {
		t.Fatalf("postgres DSN = %q, want existing db/password", values["CLIRELAY_POSTGRES_DSN"])
	}
}

func TestComposeInitEnvCreatesMissingConfigFromExample(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	configFile := filepath.Join(dir, "config.yaml")
	exampleFile := filepath.Join(dir, "config.example.yaml")
	if err := os.WriteFile(exampleFile, []byte("host: \"\"\nport: 8317\n"), 0o644); err != nil {
		t.Fatalf("write config example: %v", err)
	}

	cmd := exec.Command("sh", "scripts/init-compose-env.sh")
	cmd.Env = append(os.Environ(),
		"CLIRELAY_ENV_FILE="+envFile,
		"CLIRELAY_PROJECT_DIR="+dir,
		"CLIRELAY_CONFIG_FILE="+configFile,
		"CLIRELAY_CONFIG_EXAMPLE_FILE="+exampleFile,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init-compose-env failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	configData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if string(configData) != "host: \"\"\nport: 8317\n" {
		t.Fatalf("generated config = %q", configData)
	}

	if err := os.WriteFile(configFile, []byte("custom: true\n"), 0o600); err != nil {
		t.Fatalf("write custom config: %v", err)
	}
	cmd = exec.Command("sh", "scripts/init-compose-env.sh")
	cmd.Env = append(os.Environ(),
		"CLIRELAY_ENV_FILE="+envFile,
		"CLIRELAY_PROJECT_DIR="+dir,
		"CLIRELAY_CONFIG_FILE="+configFile,
		"CLIRELAY_CONFIG_EXAMPLE_FILE="+exampleFile,
	)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("second init-compose-env failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	configData, err = os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read custom config: %v", err)
	}
	if string(configData) != "custom: true\n" {
		t.Fatalf("custom config was overwritten: %q", configData)
	}
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values
}

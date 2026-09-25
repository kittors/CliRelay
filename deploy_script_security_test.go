package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The deploy scripts run as root and the deploy user reaches them through sudo.
// They used to take RECONCILE_SCRIPT, CLEANUP_SCRIPT, BASE_DIR and friends from
// the caller's environment, which under the SETENV sudo rule let the deploy
// user run any script as root. These tests pin the fix.

// callerTunables are the only values the root scripts may take from the
// caller's environment: the ones sudoers passes through env_keep.
var callerTunables = []string{
	"COMMIT_SHA",
	"EXPECTED_SCRIPT_VERSION",
	"SERVICE_CPU_QUOTA",
	"SERVICE_GO_MEM_LIMIT",
	"SERVICE_MEMORY_HIGH",
	"SERVICE_MEMORY_MAX",
	"SERVICE_TASKS_MAX",
}

// runShellFunctions defines the named functions exactly as the script ships
// them, then runs body, so tests exercise the code the deploy host runs.
func runShellFunctions(t *testing.T, script string, funcs []string, body string, env ...string) (string, error) {
	t.Helper()
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	for _, name := range funcs {
		b.WriteString(extractShellFunction(t, script, name))
		b.WriteString("\n")
	}
	b.WriteString(body)
	cmd := exec.Command("bash", "-c", b.String())
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDeployScriptIgnoresCallerOverridesOfRootPaths(t *testing.T) {
	if _, err := os.Stat("/etc/clirelay2/deploy.env"); err == nil {
		t.Skip("this machine has a real /etc/clirelay2/deploy.env")
	}
	hostile := []string{
		"RECONCILE_SCRIPT=/tmp/evil-reconcile.sh",
		"CLEANUP_SCRIPT=/tmp/evil-cleanup.sh",
		"BASE_DIR=/tmp/evil",
		"SERVICE_NAME=evil",
		"TEMP_BIN=/tmp/evil-bin",
		"ACTIVE_PORT_FILE=/tmp/evil-port",
		"CLIRELAY_ENV_FILE=/tmp/evil.env",
		"APP_ENV_FILE=/tmp/evil.env",
		"DEPLOY_ENV_FILE=/tmp/evil-deploy.env",
		"NGINX_CONF=/tmp/evil.conf",
		"NGINX_SEARCH_DIRS=/tmp",
		"NGINX_BODY_SIZE_CONF=/etc/shadow",
		"SLOT_USER=root",
		"PORT_A=22",
		"GO_MEM_LIMIT_PERCENT=a[$(touch /tmp/evil)]",
		"SCRIPT_VERSION=anything",
	}
	cmd := exec.Command("bash", "scripts/deploy-blue-green.sh", "--print-settings")
	cmd.Env = append(os.Environ(), hostile...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("print settings: %v\n%s", err, out)
	}
	settings := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		settings[key] = value
	}
	for key, want := range map[string]string{
		"RECONCILE_SCRIPT":     "/opt/clirelay2/scripts/reconcile-active-slot.sh",
		"CLEANUP_SCRIPT":       "/opt/clirelay2/scripts/cleanup-drained-slot.sh",
		"BASE_DIR":             "/opt/clirelay2",
		"SERVICE_NAME":         "clirelay2",
		"TEMP_BIN":             "/opt/clirelay2/cli-proxy-api-new",
		"ACTIVE_PORT_FILE":     "/opt/clirelay2/.active-port",
		"APP_ENV_FILE":         "/opt/clirelay2/.env",
		"DEPLOY_ENV_FILE":      "/etc/clirelay2/deploy.env",
		"NGINX_CONF":           "",
		"SLOT_USER":            "",
		"PORT_A":               "8318",
		"GO_MEM_LIMIT_PERCENT": "85",
		"SCRIPT_VERSION":       extractShellAssignment(t, "scripts/deploy-blue-green.sh", "SCRIPT_VERSION"),
	} {
		if got, ok := settings[key]; !ok || got != want {
			t.Fatalf("%s = %q (present %v) with a hostile environment, want %q\n%s", key, got, ok, want, out)
		}
	}

	// Tunables do come from the caller, so they are validated instead: a
	// newline would add a directive such as ExecStartPre=+ to the root-run unit.
	cmd = exec.Command("bash", "scripts/deploy-blue-green.sh", "--print-settings")
	cmd.Env = append(os.Environ(), "SERVICE_CPU_QUOTA=170%\nExecStartPre=+/tmp/evil")
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing SERVICE_CPU_QUOTA") {
		t.Fatalf("a tunable carrying a unit directive must be refused, err=%v\n%s", err, out)
	}
}

// Every setting the root scripts take from their environment is a line of the
// form NAME="${NAME:-default}". Only the whitelisted tunables may appear so;
// a root path read this way is the privilege escalation this guards against.
// Lower-case names are the scripts' own intermediate values, assigned just
// before they are defaulted.
func TestDeployScriptTakesOnlyTunablesFromTheEnvironment(t *testing.T) {
	selfDefault := regexp.MustCompile(`(?m)^\s*([A-Z_][A-Z0-9_]*)="?\$\{([A-Z_][A-Z0-9_]*)(:?[-=?+])`)
	for _, path := range []string{"scripts/deploy-blue-green.sh", "scripts/clirelay-gha-deploy"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var fromEnv []string
		for _, m := range selfDefault.FindAllStringSubmatch(string(data), -1) {
			if m[1] == m[2] {
				fromEnv = append(fromEnv, m[1])
			}
		}
		sort.Strings(fromEnv)
		allowed := map[string]bool{}
		for _, name := range callerTunables {
			allowed[name] = true
		}
		for _, name := range fromEnv {
			if !allowed[name] {
				t.Fatalf("%s reads %s from the caller's environment; only %v may come from there", path, name, callerTunables)
			}
		}
	}

	data, err := os.ReadFile("scripts/clirelay-gha-deploy")
	if err != nil {
		t.Fatalf("read entrypoint: %v", err)
	}
	entry := string(data)
	for _, want := range []string{
		// bash -p ignores BASH_ENV, ENV, SHELLOPTS and exported functions.
		"#!/bin/bash -p\n",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
		// The deploy script gets the validated tunables and nothing else.
		`exec env -i PATH="$SAFE_PATH"`,
		"TUNABLES='" + strings.Join([]string{"COMMIT_SHA", "EXPECTED_SCRIPT_VERSION", "SERVICE_CPU_QUOTA", "SERVICE_MEMORY_HIGH", "SERVICE_MEMORY_MAX", "SERVICE_TASKS_MAX", "SERVICE_GO_MEM_LIMIT"}, " ") + "'",
		`require_root_controlled "${SCRIPTS}/${script}"`,
		`setenv-probe-accepted`,
		`1:--preflight) mode=preflight ;;`,
		// Copy the staged binary without following links, then check the copy.
		`cp -P -- "$STAGED" "${stage_dir}/bin"`,
		`grep -a -F -q "$COMMIT_SHA" "${stage_dir}/bin"`,
	} {
		if !strings.Contains(entry, want) {
			t.Fatalf("entrypoint missing guard %q", want)
		}
	}
}

// Both root scripts validate the tunables: the entrypoint before handing them
// on, the deploy script before writing them into the unit. The tables must not
// drift apart.
func TestDeployTunableValidationMatchesEntrypoint(t *testing.T) {
	cases := []struct {
		name, value string
		valid       bool
	}{
		{"COMMIT_SHA", strings.Repeat("a1", 20), true},
		{"COMMIT_SHA", "", true},
		{"COMMIT_SHA", ".", false},
		{"COMMIT_SHA", strings.Repeat("A1", 20), false},
		{"EXPECTED_SCRIPT_VERSION", "2026.09.25.1", true},
		{"EXPECTED_SCRIPT_VERSION", "1 2", false},
		{"SERVICE_CPU_QUOTA", "170%", true},
		{"SERVICE_CPU_QUOTA", "", true},
		{"SERVICE_CPU_QUOTA", "170", false},
		{"SERVICE_CPU_QUOTA", "170%\nExecStartPre=+/tmp/evil", false},
		{"SERVICE_MEMORY_HIGH", "1400M", true},
		{"SERVICE_MEMORY_MAX", "infinity", true},
		{"SERVICE_MEMORY_MAX", "1.5G", false},
		{"SERVICE_MEMORY_HIGH", "1400M\nUser=root", false},
		{"SERVICE_TASKS_MAX", "512", true},
		{"SERVICE_TASKS_MAX", "512;", false},
		{"SERVICE_GO_MEM_LIMIT", "", true},
		{"SERVICE_GO_MEM_LIMIT", "1258291200", true},
		{"SERVICE_GO_MEM_LIMIT", "1200MiB", true},
		{"SERVICE_GO_MEM_LIMIT", "1200M", false},
		{"RECONCILE_SCRIPT", "/tmp/evil.sh", false},
	}
	var body strings.Builder
	var env []string
	for i, c := range cases {
		env = append(env, fmt.Sprintf("CASE_%d_NAME=%s", i, c.name), fmt.Sprintf("CASE_%d_VALUE=%s", i, c.value))
		fmt.Fprintf(&body, "n=CASE_%d_NAME; v=CASE_%d_VALUE; if tunable_is_valid \"${!n}\" \"${!v}\"; then echo %d:valid; else echo %d:invalid; fi\n", i, i, i, i)
	}
	for _, script := range []string{"scripts/deploy-blue-green.sh", "scripts/clirelay-gha-deploy"} {
		out, err := runShellFunctions(t, script, []string{"tunable_is_valid"}, body.String(), env...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		for i, c := range cases {
			want := fmt.Sprintf("%d:invalid", i)
			if c.valid {
				want = fmt.Sprintf("%d:valid", i)
			}
			if !strings.Contains(out, want+"\n") {
				t.Fatalf("%s: tunable_is_valid(%s, %q) should be %v\n%s", script, c.name, c.value, c.valid, out)
			}
		}
	}
}

func TestDeployEnvFileIsParsedNotSourced(t *testing.T) {
	keys := extractShellAssignment(t, "scripts/deploy-blue-green.sh", "DEPLOY_ENV_KEYS")
	funcs := []string{"fail", "setting_is_valid", "load_deploy_env"}
	run := func(t *testing.T, content string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "deploy.env")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write deploy.env: %v", err)
		}
		body := fmt.Sprintf(`DEPLOY_ENV_KEYS=%q
DOMAIN=relay.07230805.xyz NODE_PUBLIC_IP= NGINX_CONF= SLOT_USER= DRAIN_SECONDS=180
cd %q
load_deploy_env %q
printf 'DOMAIN=%%s NODE_PUBLIC_IP=%%s NGINX_CONF=%%s SLOT_USER=%%s DRAIN_SECONDS=%%s\n' "$DOMAIN" "$NODE_PUBLIC_IP" "$NGINX_CONF" "$SLOT_USER" "$DRAIN_SECONDS"
`, keys, dir, path)
		out, err := runShellFunctions(t, "scripts/deploy-blue-green.sh", funcs, body)
		return out, dir, err
	}

	out, _, err := run(t, "# node n43\nDOMAIN=relay.example.test\nNODE_PUBLIC_IP=\"203.0.113.10\"\r\n  NGINX_CONF='/etc/nginx/conf.d/relay.conf'\nSLOT_USER=clirelay\nDRAIN_SECONDS=240")
	if err != nil {
		t.Fatalf("load a valid deploy.env: %v\n%s", err, out)
	}
	if want := "DOMAIN=relay.example.test NODE_PUBLIC_IP=203.0.113.10 NGINX_CONF=/etc/nginx/conf.d/relay.conf SLOT_USER=clirelay DRAIN_SECONDS=240"; !strings.Contains(out, want) {
		t.Fatalf("deploy.env values = %q, want %q", out, want)
	}

	for content, reason := range map[string]string{
		"RECONCILE_SCRIPT=/tmp/evil.sh\n": "unknown setting RECONCILE_SCRIPT",
		"DOMAIN=$(touch marker)\n":        "invalid DOMAIN",
		"DRAIN_SECONDS=0\n":               "invalid DRAIN_SECONDS",
		"SLOT_USER=root;id\n":             "invalid SLOT_USER",
		"just words\n":                    "expected KEY=value",
	} {
		out, dir, err := run(t, content)
		if err == nil || !strings.Contains(out, reason) {
			t.Fatalf("deploy.env %q must be refused with %q, err=%v\n%s", content, reason, err, out)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "marker")); statErr == nil {
			t.Fatalf("a deploy.env value was executed: %q", content)
		}
	}
}

// The workflow sends one repository-wide set of resource limits, but the nodes
// differ: n43 has 3.9G and also runs PostgreSQL, etcd and Redis, n156 has 8G.
// A limit in the node's root-owned deploy.env beats the workflow's value.
func TestDeployEnvResourceLimitsOverrideTheWorkflow(t *testing.T) {
	keys := extractShellAssignment(t, "scripts/deploy-blue-green.sh", "DEPLOY_ENV_KEYS")
	funcs := []string{"fail", "tunable_is_valid", "setting_is_valid", "read_caller_tunables", "load_deploy_env", "systemd_bytes", "derive_go_mem_limit"}
	// What the workflow passes through sudo when the repository variables are set.
	workflowEnv := []string{
		"SERVICE_CPU_QUOTA=170%",
		"SERVICE_MEMORY_HIGH=1400M",
		"SERVICE_MEMORY_MAX=1600M",
		"SERVICE_TASKS_MAX=512",
		"SERVICE_GO_MEM_LIMIT=1200MiB",
	}
	run := func(t *testing.T, content string) (string, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "deploy.env")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write deploy.env: %v", err)
		}
		// Same order as the script: caller tunables, then the node file.
		body := fmt.Sprintf(`DEPLOY_ENV_KEYS=%q
GO_MEM_LIMIT_PERCENT=85
deploy_env_set=""
read_caller_tunables
load_deploy_env %q
derive_go_mem_limit
echo "limits=${SERVICE_CPU_QUOTA} ${SERVICE_MEMORY_HIGH} ${SERVICE_MEMORY_MAX} ${SERVICE_TASKS_MAX} ${SERVICE_GO_MEM_LIMIT}"
`, keys, path)
		return runShellFunctions(t, "scripts/deploy-blue-green.sh", funcs, body, workflowEnv...)
	}

	for content, want := range map[string]string{
		// n43's current slot units.
		"SERVICE_CPU_QUOTA=120%\nSERVICE_MEMORY_HIGH=900M\nSERVICE_MEMORY_MAX=1100M\nSERVICE_GO_MEM_LIMIT=734003200\n": "limits=120% 900M 1100M 512 734003200",
		// A node that lowers MemoryHigh without pinning GOMEMLIMIT must not keep
		// the workflow's 1200MiB, which sits above its MemoryHigh: it gets 85%
		// of its own 900M instead.
		"SERVICE_MEMORY_HIGH=900M\n":                        "limits=170% 900M 1600M 512 802160640",
		"SERVICE_MEMORY_HIGH=900M\nSERVICE_GO_MEM_LIMIT=\n": "limits=170% 900M 1600M 512 802160640",
		// Without resource keys the workflow's values stand.
		"DOMAIN=relay.example.test\n":  "limits=170% 1400M 1600M 512 1200MiB",
		"SERVICE_TASKS_MAX=infinity\n": "limits=170% 1400M 1600M infinity 1200MiB",
	} {
		out, err := run(t, content)
		if err != nil || !strings.Contains(out, want+"\n") {
			t.Fatalf("deploy.env %q: want %q, err=%v\n%s", content, want, err, out)
		}
	}

	for content, reason := range map[string]string{
		"SERVICE_CPU_QUOTA=120\n":                             "invalid SERVICE_CPU_QUOTA",
		"SERVICE_MEMORY_MAX=1.1G\n":                           "invalid SERVICE_MEMORY_MAX",
		"SERVICE_MEMORY_HIGH=\n":                              "invalid SERVICE_MEMORY_HIGH",
		"SERVICE_TASKS_MAX=512;\n":                            "invalid SERVICE_TASKS_MAX",
		"SERVICE_GO_MEM_LIMIT=700M\n":                         "invalid SERVICE_GO_MEM_LIMIT",
		"SERVICE_MEMORY_HIGH=900M\nExecStartPre=+/tmp/evil\n": "unknown setting ExecStartPre",
	} {
		out, err := run(t, content)
		if err == nil || !strings.Contains(out, reason) {
			t.Fatalf("deploy.env %q must be refused with %q, err=%v\n%s", content, reason, err, out)
		}
	}
}

// A root-owned file inside a directory the deploy user can write is not
// trusted: the directory lets the file be replaced.
func TestDeployRootControlledCheckWalksEveryParent(t *testing.T) {
	for script, funcs := range map[string][]string{
		"scripts/deploy-blue-green.sh": {"fail", "canonical_path", "require_root_controlled"},
		"scripts/clirelay-gha-deploy":  {"die", "canonical_path", "require_root_controlled"},
	} {
		if out, err := runShellFunctions(t, script, funcs, "require_root_controlled /etc/hosts && echo trusted"); err != nil || !strings.Contains(out, "trusted") {
			t.Fatalf("%s: /etc/hosts should count as root-controlled: %v\n%s", script, err, out)
		}
		if os.Geteuid() == 0 {
			continue
		}
		mine := filepath.Join(t.TempDir(), "deploy.env")
		if err := os.WriteFile(mine, []byte("DOMAIN=relay.example.test\n"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
		out, err := runShellFunctions(t, script, funcs, "require_root_controlled "+mine)
		if err == nil || !strings.Contains(out, "must be owned by root") {
			t.Fatalf("%s: a file this user owns must not be trusted, err=%v\n%s", script, err, out)
		}
	}
}

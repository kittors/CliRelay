package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These are configuration drift guard tests: they assert shipped workflow text,
// not runtime behavior.
func TestDeployWorkflowOnlyPublishesBackendBinary(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		`Upload binary to staging`,
		`/opt/clirelay2/incoming/cli-proxy-api-new`,
		`User deploy`,
		`/usr/local/sbin/clirelay-gha-deploy`,
		`StrictHostKeyChecking yes`,
		`DEPLOY_SSH_KNOWN_HOSTS`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy workflow missing backend binary deployment marker %q", want)
		}
	}

	for _, forbidden := range []string{
		`Upload panel assets`,
		`source: "manage.html,management.html,assets"`,
		`scripts/migrate-sqlite-to-postgres.sh`,
		`scripts/prepare-runtime-data-stack.sh`,
		`PANEL_SRC=`,
		`PANEL_DIR=`,
		`relay-panel`,
		`/home/web/html`,
		`appleboy/scp-action`,
		`appleboy/ssh-action`,
		`username: root`,
		`StrictHostKeyChecking accept-new`,
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("backend deploy workflow must not publish frontend panel assets or use insecure deploy markers, found %q", forbidden)
		}
	}
}

func TestDeployWorkflowUsesBlueGreenDeployment(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		`Blue-green deploy via fixed root entrypoint`,
		`SERVICE_CPU_QUOTA: ${{ vars.CLIRELAY_SERVICE_CPU_QUOTA || '170%' }}`,
		`SERVICE_MEMORY_HIGH: ${{ vars.CLIRELAY_SERVICE_MEMORY_HIGH || '1400M' }}`,
		`SERVICE_MEMORY_MAX: ${{ vars.CLIRELAY_SERVICE_MEMORY_MAX || '1600M' }}`,
		`SERVICE_TASKS_MAX: ${{ vars.CLIRELAY_SERVICE_TASKS_MAX || '512' }}`,
		`COMMIT_SHA: ${{ github.sha }}`,
		`/usr/local/sbin/clirelay-gha-deploy`,
		`EXPECTED_SCRIPT_VERSION`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy workflow missing blue-green marker %q", want)
		}
	}

	for _, forbidden := range []string{
		`systemctl stop clirelay2`,
		`systemctl start clirelay2`,
		`Stop, swap, and restart`,
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("deploy workflow still has outage-prone restart marker %q", forbidden)
		}
	}
}

func TestBlueGreenDeployScriptSyntaxAndGuards(t *testing.T) {
	for _, path := range []string{
		"scripts/deploy-blue-green.sh",
		"scripts/cleanup-drained-slot.sh",
		"scripts/reconcile-active-slot.sh",
	} {
		cmd := exec.Command("bash", "-n", path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s syntax failed: %v\n%s", path, err, out)
		}
	}

	data, err := os.ReadFile("scripts/deploy-blue-green.sh")
	if err != nil {
		t.Fatalf("read deploy script: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`/readyz`,
		`/healthz`,
		`CLIRELAY_PORT=`,
		`.active-port`,
		`reconcile-active-slot.sh`,
		`HEALTH_TIMEOUT_SECONDS`,
		`SMOKE_TIMEOUT_SECONDS`,
		`PUBLIC_BASE_URL`,
		`external smoke failed`,
		`rolling nginx back`,
		`MIN_AVAILABLE_MB`,
		`NGINX_CONTAINER`,
		`EnvironmentFile=`,
		`docker exec "$NGINX_CONTAINER" nginx -t`,
		`nginx -t`,
		`DRAIN_SECONDS`,
		`systemd-run`,
		`scripts/cleanup-drained-slot.sh`,
		`NGINX_BACKUP_PATTERN`,
		`SCRIPT_VERSION`,
		`TimeoutStopSec=${SHUTDOWN_GRACE_SECONDS}`,
		`CLIRELAY_SHUTDOWN_GRACE`,
		// The cutover must prove it happened rather than infer it from an exit
		// status. A missing rewrite tool once left the config untouched while
		// the deploy reported success, which is what manufactured the slot
		// drift behind two outages.
		`rewrite_slot_port`,
		`nginx_slot_port`,
		`required command not found on deploy host`,
		`refusing to stop`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy script missing guard %q", want)
		}
	}
	for _, forbidden := range []string{
		// perl is not installed on the deploy host. Using it for the config
		// rewrite failed silently and the deploy still reported success.
		`perl -0pi`,
		// The dot-anchored backup filter only caught `.bak.<timestamp>` and let
		// every `.bak-before-<tag>` file through, so the lookup could return a
		// backup vhost that nginx never reads. Replaced by NGINX_BACKUP_PATTERN;
		// see TestNginxBackupPatternExcludesEveryBackupShape.
		`grep -v '\.bak\.'`,
		`migrate-sqlite-to-postgres.sh`,
		`Legacy SQLite`,
		`stop_active_units_for_migration`,
		`CLIRELAY_SQLITE_PATH`,
		`usage.db`,
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("deploy script must not run legacy SQLite migration during blue-green deploy, found %q", forbidden)
		}
	}
}

func TestCleanupDrainedSlotStopsOnlyTheExpectedInactiveSlot(t *testing.T) {
	tmp := t.TempDir()
	binDir := tmp + "/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	logPath := tmp + "/systemctl.log"
	activeUnitPath := tmp + "/active-unit"
	fakeSystemctl := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
if [ "$1" = "is-active" ] && [ "$2" = "--quiet" ]; then
  active="$(cat "$SYSTEMCTL_ACTIVE_UNIT" 2>/dev/null || true)"
  if [ "${3:-}" = "$active" ]; then
    exit 0
  fi
  exit 3
fi
`
	if err := os.WriteFile(binDir+"/systemctl", []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}
	activePortFile := tmp + "/.active-port"
	if err := os.WriteFile(activePortFile, []byte("8319\n"), 0o644); err != nil {
		t.Fatalf("write active port: %v", err)
	}
	if err := os.WriteFile(activeUnitPath, []byte("clirelay2-8319\n"), 0o644); err != nil {
		t.Fatalf("write active unit: %v", err)
	}

	runCleanup := func() string {
		t.Helper()
		cmd := exec.Command("bash", "scripts/cleanup-drained-slot.sh", "8318", "8319")
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"SYSTEMCTL_LOG="+logPath,
			"SYSTEMCTL_ACTIVE_UNIT="+activeUnitPath,
			"BASE_DIR="+tmp,
			"ACTIVE_PORT_FILE="+activePortFile,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cleanup drained slot: %v\n%s", err, out)
		}
		return string(out)
	}

	runCleanup()
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl log: %v", err)
	}
	logText := string(logData)
	for _, want := range []string{"disable --now clirelay2", "disable --now clirelay2-8318"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("cleanup log missing %q: %s", want, logText)
		}
	}

	if err := os.WriteFile(activePortFile, []byte("8318\n"), 0o644); err != nil {
		t.Fatalf("move active port back: %v", err)
	}
	if err := os.Remove(logPath); err != nil {
		t.Fatalf("clear systemctl log: %v", err)
	}
	if out := runCleanup(); !strings.Contains(out, "Skip draining 8318") {
		t.Fatalf("expected stale cleanup to be skipped, got: %s", out)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("stale cleanup must not call systemctl, stat err = %v", err)
	}
}

func TestReconcileActiveSlotRepairsStaleMarker(t *testing.T) {
	tmp := t.TempDir()
	binDir := tmp + "/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("create fake bin dir: %v", err)
	}
	statePath := tmp + "/active-unit"
	activePortFile := tmp + "/.active-port"
	fakeSystemctl := `#!/usr/bin/env bash
if [ "$1" = "is-active" ] && [ "$2" = "--quiet" ]; then
  active="$(cat "$SYSTEMCTL_ACTIVE_UNIT" 2>/dev/null || true)"
  if [ "${3:-}" = "$active" ]; then
    exit 0
  fi
  exit 3
fi
exit 1
`
	if err := os.WriteFile(binDir+"/systemctl", []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}
	if err := os.WriteFile(activePortFile, []byte("8318\n"), 0o644); err != nil {
		t.Fatalf("write active port: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("clirelay2-8319\n"), 0o644); err != nil {
		t.Fatalf("write active unit: %v", err)
	}

	cmd := exec.Command("bash", "scripts/reconcile-active-slot.sh")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"BASE_DIR="+tmp,
		"ACTIVE_PORT_FILE="+activePortFile,
		"SYSTEMCTL_ACTIVE_UNIT="+statePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reconcile active slot: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	got := strings.TrimSpace(lines[len(lines)-1])
	if got != "8319" {
		t.Fatalf("reconciled slot = %q, want 8319", got)
	}
	data, err := os.ReadFile(activePortFile)
	if err != nil {
		t.Fatalf("read active port file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "8319" {
		t.Fatalf("active port file = %q, want 8319", got)
	}
}

func TestDeployCompletesBeforeDispatchingDevDockerBuild(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	content := string(data)
	deployIndex := strings.Index(content, `name: Blue-green deploy via fixed root entrypoint`)
	dockerIndex := strings.Index(content, `name: Trigger dev Docker image build`)
	if deployIndex < 0 || dockerIndex < 0 || dockerIndex <= deployIndex {
		t.Fatalf("dev Docker build must be dispatched only after blue-green deployment")
	}
	if !strings.Contains(content, `if: success() && env.SHOULD_DEPLOY == 'true'`) {
		t.Fatalf("docker publish must only run after a real deploy attempt")
	}
	for _, want := range []string{
		`actions: write`,
		`cancel-in-progress: false`,
		`gh workflow run docker-publish.yml`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy workflow missing deployment-priority marker %q", want)
		}
	}
}

func TestReleaseAndDeployWorkflowsRejectVendoredPanelAssets(t *testing.T) {
	for _, path := range []string{
		".github/workflows/deploy.yml",
		".github/workflows/docker-publish.yml",
		".github/workflows/release.yaml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(data), `./scripts/ensure-no-vendored-panel-assets.sh`) {
			t.Fatalf("%s must reject committed frontend panel build output", path)
		}
	}

	data, err := os.ReadFile(".github/workflows/pr-test-build.yml")
	if err != nil {
		t.Fatalf("read PR workflow: %v", err)
	}
	if !strings.Contains(string(data), `./scripts/ci-pr.sh`) {
		t.Fatalf("PR workflow must use the shared PR check script")
	}
	data, err = os.ReadFile("scripts/ci-pr.sh")
	if err != nil {
		t.Fatalf("read PR check script: %v", err)
	}
	if !strings.Contains(string(data), `./scripts/ensure-no-vendored-panel-assets.sh`) {
		t.Fatalf("PR check script must reject committed frontend panel build output")
	}
}

func TestDockerPublishWorkflowUsesGHCRForBranchesAndReleaseTags(t *testing.T) {
	if _, err := os.Stat(".github/workflows/docker-image.yml"); !os.IsNotExist(err) {
		t.Fatalf("legacy DockerHub workflow must be removed, stat err = %v", err)
	}

	data, err := os.ReadFile(".github/workflows/docker-publish.yml")
	if err != nil {
		t.Fatalf("read Docker publish workflow: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"tags:\n      - 'v*'",
		"REGISTRY: ghcr.io",
		"IMAGE_NAME: kittors/clirelay",
		`if [[ "${GITHUB_REF_TYPE:-branch}" == "tag" ]]; then`,
		`FRONTEND_REF="main"`,
		`VERSION="${REF_NAME}"`,
		"type=ref,event=tag",
		"github.ref_name == 'main' || github.ref_type == 'tag'",
		"branches: [main]",
		"group: docker-publish-${{ github.ref }}",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("Docker publish workflow missing GHCR release marker %q", want)
		}
	}

	for _, forbidden := range []string{
		"branches: [main, dev]",
		"DOCKERHUB_USERNAME",
		"DOCKERHUB_TOKEN",
		"eceasy/cli-proxy-api",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Docker publish workflow still contains legacy DockerHub marker %q", forbidden)
		}
	}
}

// extractShellAssignment pulls a single-quoted assignment out of a shell script
// so the test exercises the value the deploy host will actually use, rather
// than a copy that can drift away from it.
func extractShellAssignment(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name+"=") {
			continue
		}
		value := strings.TrimPrefix(line, name+"=")
		value = strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'")
		if value != "" {
			return value
		}
	}
	t.Fatalf("%s does not define %s", path, name)
	return ""
}

// The deploy host keeps every historical vhost next to the live one (74 files
// at the time of writing), all containing the same server_name. Picking a
// backup means the cutover rewrites a file nginx never reads: traffic stays on
// the old slot, the deploy reports success, and the drain step then stops the
// slot that is still serving. This asserts the real grep behaviour against the
// filename shapes observed on the host.
func TestNginxBackupPatternExcludesEveryBackupShape(t *testing.T) {
	pattern := extractShellAssignment(t, "scripts/deploy-blue-green.sh", "NGINX_BACKUP_PATTERN")

	dir := t.TempDir()
	const live = "code.07230805.xyz.conf"
	backups := []string{
		"code.07230805.xyz.conf.bak.1787125852",
		"code.07230805.xyz.conf.bak.20260819_152834",
		"code.07230805.xyz.conf.bak-before-options",
		"code.07230805.xyz.conf.bak-before-restore-001802",
		"code.07230805.xyz.conf.bak",
	}
	for _, name := range append([]string{live}, backups...) {
		if err := os.WriteFile(dir+"/"+name, []byte("server_name code.07230805.xyz;\nproxy_pass http://127.0.0.1:8319;\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	script := `set -o pipefail; grep -Rsl "$DOMAIN" "$DIR" 2>/dev/null | grep -Ev "$PATTERN" || true`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "DOMAIN=code.07230805.xyz", "DIR="+dir, "PATTERN="+pattern)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run lookup: %v\n%s", err, out)
	}

	matches := strings.Fields(strings.TrimSpace(string(out)))
	if len(matches) != 1 {
		t.Fatalf("lookup matched %d files, want only the live vhost: %v", len(matches), matches)
	}
	if got := matches[0]; got != dir+"/"+live {
		t.Fatalf("lookup selected %q, want the live vhost %q", got, dir+"/"+live)
	}
}

// A cutover that failed to move nginx leaves .active-port and systemd agreeing
// with each other while nginx still points at the old slot. Draining then takes
// production down, so the drain must refuse.
func TestCleanupDrainedSlotRefusesToStopSlotNginxStillServes(t *testing.T) {
	tmp := t.TempDir()
	binDir := tmp + "/bin"
	nginxDir := tmp + "/nginx"
	for _, dir := range []string{binDir, nginxDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	activeUnitPath := tmp + "/active-unit"
	fakeSystemctl := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
if [ "$1" = "is-active" ] && [ "$2" = "--quiet" ]; then
  active="$(cat "$SYSTEMCTL_ACTIVE_UNIT" 2>/dev/null || true)"
  if [ "${3:-}" = "$active" ]; then
    exit 0
  fi
  exit 3
fi
`
	if err := os.WriteFile(binDir+"/systemctl", []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}

	activePortFile := tmp + "/.active-port"
	// Both local sources say 8319 won the cutover...
	if err := os.WriteFile(activePortFile, []byte("8319\n"), 0o644); err != nil {
		t.Fatalf("write active port: %v", err)
	}
	if err := os.WriteFile(activeUnitPath, []byte("clirelay2-8319\n"), 0o644); err != nil {
		t.Fatalf("write active unit: %v", err)
	}
	// ...but nginx never moved off 8318.
	if err := os.WriteFile(nginxDir+"/live.conf", []byte("proxy_pass http://127.0.0.1:8318;\n"), 0o644); err != nil {
		t.Fatalf("write live vhost: %v", err)
	}
	// A stale backup pointing at the new slot must not be mistaken for live config.
	if err := os.WriteFile(nginxDir+"/live.conf.bak-before-cutover", []byte("proxy_pass http://127.0.0.1:8319;\n"), 0o644); err != nil {
		t.Fatalf("write backup vhost: %v", err)
	}

	logPath := tmp + "/systemctl.log"
	cmd := exec.Command("bash", "scripts/cleanup-drained-slot.sh", "8318", "8319")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"SYSTEMCTL_LOG="+logPath,
		"SYSTEMCTL_ACTIVE_UNIT="+activeUnitPath,
		"BASE_DIR="+tmp,
		"ACTIVE_PORT_FILE="+activePortFile,
		"NGINX_SEARCH_DIRS="+nginxDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("drain succeeded while nginx still served 8318; output:\n%s", out)
	}
	if !strings.Contains(string(out), "live nginx still proxies to it") {
		t.Fatalf("drain refused for the wrong reason:\n%s", out)
	}

	logData, _ := os.ReadFile(logPath)
	if strings.Contains(string(logData), "disable --now clirelay2-8318") {
		t.Fatalf("drain stopped the slot nginx was serving:\n%s", logData)
	}
}

// The gate compares the workflow's expectation against the version baked into
// the script. When they drift, every deploy fails before touching the host --
// safe, but it silently disables deployment until someone notices.
func TestDeployScriptVersionMatchesWorkflowExpectation(t *testing.T) {
	raw := extractShellAssignment(t, "scripts/deploy-blue-green.sh", "SCRIPT_VERSION")
	raw = strings.Trim(raw, `"`)
	scriptVersion := strings.TrimSuffix(strings.TrimPrefix(raw, "${SCRIPT_VERSION:-"), "}")
	if scriptVersion == "" || strings.ContainsAny(scriptVersion, "${}\"") {
		t.Fatalf("could not parse SCRIPT_VERSION, got %q", raw)
	}

	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	want := "EXPECTED_SCRIPT_VERSION: '" + scriptVersion + "'"
	if !strings.Contains(string(data), want) {
		t.Fatalf("deploy workflow must expect script version %q (looked for %q)", scriptVersion, want)
	}
}

// Deployment must not depend on a repository variable being present. It used to
// require CLIRELAY_DEV_AUTO_DEPLOY == 'true', so clearing or losing that
// variable turned every merge into dev into a silent build-only run while the
// workflow still reported success.
func TestDeployIsOptOutRatherThanOptIn(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `vars.CLIRELAY_DEV_AUTO_DEPLOY != 'false'`) {
		t.Fatalf("merges into dev must deploy unless explicitly disabled")
	}
	if strings.Contains(content, `vars.CLIRELAY_DEV_AUTO_DEPLOY == 'true'`) {
		t.Fatalf("deployment must not be gated behind an opt-in variable")
	}
}

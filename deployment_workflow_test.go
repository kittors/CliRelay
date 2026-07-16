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
		`grep -v '\.bak\.'`,
		`SCRIPT_VERSION`,
		`TimeoutStopSec=90`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy script missing guard %q", want)
		}
	}
	for _, forbidden := range []string{
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

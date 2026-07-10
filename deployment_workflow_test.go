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
		`Upload binary and deploy script`,
		`source: "cli-proxy-api-new,scripts/deploy-blue-green.sh"`,
		`scripts/deploy-blue-green.sh`,
		`target: "/opt/clirelay2/"`,
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
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("backend deploy workflow must not publish frontend panel assets, found %q", forbidden)
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
		`Blue-green deploy`,
		`SERVICE_CPU_QUOTA="${{ vars.CLIRELAY_SERVICE_CPU_QUOTA || '170%' }}"`,
		`SERVICE_MEMORY_HIGH="${{ vars.CLIRELAY_SERVICE_MEMORY_HIGH || '1400M' }}"`,
		`SERVICE_MEMORY_MAX="${{ vars.CLIRELAY_SERVICE_MEMORY_MAX || '1600M' }}"`,
		`SERVICE_TASKS_MAX="${{ vars.CLIRELAY_SERVICE_TASKS_MAX || '512' }}"`,
		`COMMIT_SHA="${{ github.sha }}"`,
		`bash /opt/clirelay2/scripts/deploy-blue-green.sh`,
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
	cmd := exec.Command("bash", "-n", "scripts/deploy-blue-green.sh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy script syntax failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile("scripts/deploy-blue-green.sh")
	if err != nil {
		t.Fatalf("read deploy script: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`/healthz`,
		`CLIRELAY_PORT=`,
		`.active-port`,
		`HEALTH_TIMEOUT_SECONDS`,
		`MIN_AVAILABLE_MB`,
		`NGINX_CONTAINER`,
		`EnvironmentFile=`,
		`docker exec "$NGINX_CONTAINER" nginx -t`,
		`nginx -t`,
		`DRAIN_SECONDS`,
		`grep -v '\.bak\.'`,
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
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("Docker publish workflow missing GHCR release marker %q", want)
		}
	}

	for _, forbidden := range []string{
		"DOCKERHUB_USERNAME",
		"DOCKERHUB_TOKEN",
		"eceasy/cli-proxy-api",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("Docker publish workflow still contains legacy DockerHub marker %q", forbidden)
		}
	}
}

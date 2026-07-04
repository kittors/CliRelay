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
		`COMMIT_SHA="${{ github.sha }}" bash /opt/clirelay2/scripts/deploy-blue-green.sh`,
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
		`docker exec "$NGINX_CONTAINER" nginx -t`,
		`nginx -t`,
		`DRAIN_SECONDS`,
		`grep -v '\.bak\.'`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("deploy script missing guard %q", want)
		}
	}
}

func TestReleaseAndDeployWorkflowsRejectVendoredPanelAssets(t *testing.T) {
	for _, path := range []string{
		".github/workflows/pr-test-build.yml",
		".github/workflows/deploy.yml",
		".github/workflows/docker-image.yml",
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
}

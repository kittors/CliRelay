package main

import (
	"os"
	"strings"
	"testing"
)

func TestDeployWorkflowOnlyPublishesBackendBinary(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		`Upload binary (as temp name)`,
		`source: "cli-proxy-api-new"`,
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

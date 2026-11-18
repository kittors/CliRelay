// Command clirelay-compose-migrate brings a deployment's docker-compose topology and
// environment file up to what this build requires.
//
// It runs as the clirelay-init container from the newly pulled image, before any
// service is recreated, so each build migrates its own deployment forward. This is
// the counterpart to the updater's deliberate ignorance: the updater knows only that
// a preparation step exists, and this is what that step does.
package main

import (
	"log"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/deploy/composeplan"
)

func main() {
	options := composeplan.Options{
		ComposeFile: envOrDefault("CLIRELAY_COMPOSE_FILE", "/clirelay-deploy/docker-compose.yml"),
		EnvFile:     envOrDefault("CLIRELAY_ENV_FILE", "/clirelay-deploy/.env"),
		ProjectDir:  strings.TrimSpace(os.Getenv("CLIRELAY_PROJECT_DIR")),
		Service:     strings.TrimSpace(os.Getenv("CLIRELAY_TARGET_SERVICE")),
		Logf:        log.Printf,
	}

	if err := composeplan.Migrate(options); err != nil {
		// Failing here aborts the update before anything is recreated, which is the
		// safe direction: a deployment left on its previous topology still runs.
		log.Printf("compose migration failed: %v", err)
		os.Exit(1)
	}
}

func envOrDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

package main

import "os"

func migrateComposeService(target map[string]any, image string) map[string]any {
	return map[string]any{
		"image":       "${CLI_PROXY_IMAGE:-" + image + "}",
		"command":     []any{"migrate-sqlite-to-postgres.sh"},
		"entrypoint":  sourceEnvEntrypoint(),
		"environment": withoutEnvKeys(target["environment"], "CLIRELAY_SQLITE_AUTO_MIGRATE"),
		"volumes":     target["volumes"],
		"depends_on": map[string]any{
			"clirelay-init": map[string]any{"condition": "service_completed_successfully"},
			"postgres":      map[string]any{"condition": "service_healthy"},
			"redis":         map[string]any{"condition": "service_healthy"},
		},
		"restart": "no",
	}
}

func composeFileHasService(path string, service string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return hasComposeService(string(data), service)
}

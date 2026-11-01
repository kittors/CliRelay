package api

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

// reloadSecretMaskingSettings drops the cached secret masking settings of the
// tenants another node changed, or of every tenant when the events do not say
// which (a resync after missed events).
func reloadSecretMaskingSettings(events []cluster.ConfigEvent) {
	registry := contentmoderation.SecretMasking()
	if len(events) == 0 {
		registry.InvalidateAll()
		return
	}
	for _, event := range events {
		if strings.TrimSpace(event.TenantID) == "" {
			registry.InvalidateAll()
			return
		}
	}
	for _, tenant := range tenantsOf(events) {
		registry.Invalidate(tenant)
	}
}

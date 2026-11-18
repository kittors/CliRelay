package usagelogs

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// expandManagementAPIKeyFilters expands account-representative secrets to every
// owned key secret so management user filters match the aggregated counts.
func expandManagementAPIKeyFilters(tenantID string, apiKeys []string) []string {
	if len(apiKeys) == 0 {
		return nil
	}

	expanded := make([]string, 0, len(apiKeys))
	seen := make(map[string]struct{}, len(apiKeys))
	appendKey := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		expanded = append(expanded, key)
	}

	for _, key := range apiKeys {
		key = strings.TrimSpace(key)
		if key == "__system__" {
			appendKey(key)
			continue
		}
		row := usage.GetAPIKeyForTenant(tenantID, key)
		if row == nil || strings.TrimSpace(row.EndUserID) == "" {
			appendKey(key)
			continue
		}

		secrets := usage.ListAPIKeySecretsForEndUserForTenant(tenantID, row.EndUserID)
		if len(secrets) == 0 {
			appendKey(key)
			continue
		}
		for _, secret := range secrets {
			appendKey(secret)
		}
	}
	return expanded
}

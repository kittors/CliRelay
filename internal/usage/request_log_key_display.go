package usage

import "strings"

// hydrateAPIKeyDisplayNames keeps account and credential identity separate.
func hydrateAPIKeyDisplayNames(tenantID string, items []LogRow) {
	if len(items) == 0 {
		return
	}
	byID := currentAPIKeyRowsByIDForTenant(tenantID)
	byKey := currentAPIKeyRowsByKeyForTenant(tenantID)
	for i := range items {
		var row *APIKeyRow
		if r, ok := byID[strings.TrimSpace(items[i].APIKeyID)]; ok {
			copy := r
			row = &copy
		} else if r, ok := byKey[strings.TrimSpace(items[i].APIKey)]; ok {
			copy := r
			row = &copy
		} else {
			// Fall back: resolve via secret lookup when tenant maps miss.
			if live := GetAPIKey(items[i].APIKey); live != nil {
				row = live
			}
		}
		if row != nil {
			items[i].APIKeyID = strings.TrimSpace(row.ID)
			items[i].APIKeyOwnName = strings.TrimSpace(row.Name)
			items[i].EndUserDisplayName = DisplayNameForEndUser(row.EndUserID)
			if items[i].EndUserDisplayName != "" {
				items[i].APIKeyName = items[i].EndUserDisplayName
			} else if items[i].APIKeyOwnName != "" {
				items[i].APIKeyName = items[i].APIKeyOwnName
			}
		} else {
			// Legacy rows have only one snapshot label. Preserve it as the key name;
			// no account identity is inferred without a trusted ownership row.
			items[i].APIKeyOwnName = strings.TrimSpace(items[i].APIKeyName)
		}
	}
}

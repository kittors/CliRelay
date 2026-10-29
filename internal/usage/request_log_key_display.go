package usage

import "strings"

// hydrateAPIKeyDisplayNames keeps account and credential identity separate, and
// leaves every row with a key column that is safe to return: a mask or a label,
// never a secret.
func hydrateAPIKeyDisplayNames(tenantID string, items []LogRow) {
	if len(items) == 0 {
		return
	}
	byID := currentAPIKeyRowsByIDForTenant(tenantID)
	var byKey map[string]APIKeyRow
	for i := range items {
		var row *APIKeyRow
		if r, ok := byID[strings.TrimSpace(items[i].APIKeyID)]; ok {
			copy := r
			row = &copy
		} else if stored := strings.TrimSpace(items[i].APIKey); stored != "" &&
			!IsMaskedAPIKey(stored) && !IsRequestLogLabel(stored) {
			// A row that still holds a secret was written before masking and has
			// no key id yet; resolve it once so it is attributed like the rest.
			// Masked and label rows have nothing left to resolve.
			if byKey == nil {
				byKey = currentAPIKeyRowsByKeyForTenant(tenantID)
			}
			if r, ok := byKey[stored]; ok {
				copy := r
				row = &copy
			} else if live := GetAPIKey(stored); live != nil {
				// Fall back: resolve via secret lookup when tenant maps miss.
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
		// A row that belongs to a stored key holds a secret or its mask whatever the
		// text looks like; only rows without a key can carry a route/origin label.
		items[i].APIKey = storedRequestLogAPIKey(items[i].APIKey, row != nil)
		items[i].APIKeyMasked = items[i].APIKey
	}
}

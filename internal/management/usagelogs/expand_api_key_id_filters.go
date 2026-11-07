package usagelogs

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// expandManagementAPIKeyIDs expands the account representative that the
// management key facet offers into every key the account owns, so selecting an
// account lists all of its history and matches the aggregated facet count.
// Standalone key ids and the "__system__" entry pass through unchanged.
func expandManagementAPIKeyIDs(tenantID string, ids []string) []string {
	if len(ids) == 0 {
		return nil
	}

	expanded := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	appendID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		expanded = append(expanded, id)
	}

	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == usage.SystemRequestLogFilterValue {
			appendID(id)
			continue
		}
		appendID(id)
		row := usage.GetAPIKeyByIDForTenant(tenantID, id)
		if row == nil || strings.TrimSpace(row.EndUserID) == "" {
			continue
		}
		for _, sibling := range usage.ListAPIKeyIDsForEndUserForTenant(tenantID, row.EndUserID) {
			appendID(sibling)
		}
	}
	return expanded
}

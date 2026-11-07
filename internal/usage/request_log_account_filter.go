package usage

import (
	"database/sql"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// queryDistinctAPIKeyIDs returns one filter option per concrete key: the stable
// api_key_id, the key's own name and the number of matching rows. A row is
// attributed to a key by its api_key_id alone; the api_key column holds only a
// mask, so there is no secret to group by or hand back.
//
// With params.CollapseAccountKeys the keys of one end-user account fold into a
// single option (the account's default key as representative, the account's
// display name, summed counts), which is how the management UI lists them.
// The "__system__" entry counts the rows that carry no key at all (system calls).
func queryDistinctAPIKeyIDs(db *sql.DB, params LogQueryParams) ([]string, map[string]string, map[string]int64, error) {
	tenantID := normalizeTenantID(params.TenantID)
	currentByID := currentAPIKeyRowsByIDForTenant(tenantID)
	where, args := buildWhereClause(params)
	if where == "" {
		where = " WHERE"
	} else {
		where += " AND"
	}
	rows, err := db.Query(`
		SELECT
			trim(coalesce(api_key_id, '')) AS key_id,
			COALESCE(NULLIF(MAX(api_key_name), ''), '') AS snapshot_name,
			COALESCE(MAX(api_key), '') AS sample_key,
			COUNT(*) AS request_count
		FROM request_logs
		`+where+` trim(coalesce(api_key_id, '')) <> ''
		GROUP BY key_id
		ORDER BY key_id
	`, args...)
	if err != nil {
		log.Warnf("usage: distinct api_key_id groups query failed: %v", err)
		return nil, nil, nil, fmt.Errorf("usage: distinct api_key_id groups: %w", err)
	}
	defer rows.Close()

	values := make([]string, 0)
	names := make(map[string]string)
	counts := make(map[string]int64)
	for rows.Next() {
		var id, snapshotName, sampleKey string
		var requestCount int64
		if err := rows.Scan(&id, &snapshotName, &sampleKey, &requestCount); err != nil {
			log.Warnf("usage: distinct api_key_id scan failed: %v", err)
			return nil, nil, nil, err
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(snapshotName)
		// Persist-time snapshot is only the fallback for keys that no longer exist.
		if row, ok := currentByID[id]; ok {
			if own := strings.TrimSpace(row.Name); own != "" {
				name = own
			}
		}
		if name == "" {
			// An unnamed key used to be listed by its masked secret. The facet no
			// longer carries secrets, so it borrows the mask its own rows show
			// instead of an opaque id.
			name = maskedKeyLabelForFacet(sampleKey)
		}
		values = append(values, id)
		counts[id] = requestCount
		if name != "" {
			names[id] = name
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	if params.CollapseAccountKeys {
		values, names, counts = collapseAccountKeyOptions(tenantID, values, names, counts, currentByID)
	}

	var systemCount int64
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM request_logs"+where+" "+systemRequestLogPredicate(), args...,
	).Scan(&systemCount); err != nil {
		log.Warnf("usage: distinct system call count failed: %v", err)
		return nil, nil, nil, fmt.Errorf("usage: distinct system call count: %w", err)
	}
	if systemCount > 0 {
		values = append(values, SystemRequestLogFilterValue)
		counts[SystemRequestLogFilterValue] = systemCount
	}
	return values, names, counts, nil
}

// collapseAccountKeyOptions folds the options of keys owned by one end-user
// account into one option. Standalone keys stay one option each, so a "test key"
// never sits next to the account that owns it. Soft-deleted keys keep their
// ownership for the fold but never become the representative.
func collapseAccountKeyOptions(
	tenantID string,
	ids []string,
	names map[string]string,
	counts map[string]int64,
	currentByID map[string]APIKeyRow,
) ([]string, map[string]string, map[string]int64) {
	repByEndUser, nameByEndUser := accountFilterRepresentatives(tenantID)
	outIDs := make([]string, 0, len(ids))
	outNames := make(map[string]string, len(names))
	outCounts := make(map[string]int64, len(counts))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		value := id
		name := names[id]
		if row, ok := currentByID[id]; ok {
			if eu := strings.TrimSpace(row.EndUserID); eu != "" {
				if rep := strings.TrimSpace(repByEndUser[eu]); rep != "" {
					value = rep
				}
				if label := strings.TrimSpace(nameByEndUser[eu]); label != "" {
					name = label
				} else if label := ResolveAPIKeyDisplayName(&row, name); label != "" {
					name = label
				}
			}
		}
		// Sum before deduping: every retained log of the account must be counted.
		outCounts[value] += counts[id]
		if _, ok := seen[value]; ok {
			if name != "" && outNames[value] == "" {
				outNames[value] = name
			}
			continue
		}
		seen[value] = struct{}{}
		outIDs = append(outIDs, value)
		if name != "" {
			outNames[value] = name
		}
	}
	return outIDs, outNames, outCounts
}

// accountFilterRepresentatives picks one filter value per end-user: the id of the
// account's default key (else its first live key), plus the account's label.
func accountFilterRepresentatives(tenantID string) (repByEndUser map[string]string, nameByEndUser map[string]string) {
	rows := ListAPIKeysForTenant(tenantID)
	repByEndUser = make(map[string]string)
	nameByEndUser = make(map[string]string)
	defaultPicked := make(map[string]bool)
	for _, row := range rows {
		if row.Disabled {
			// Soft-deleted keys retain ownership for log collapse, but must not
			// become the account filter representative value.
			continue
		}
		eu := strings.TrimSpace(row.EndUserID)
		if eu == "" {
			continue
		}
		id := strings.TrimSpace(row.ID)
		if id == "" {
			continue
		}
		if _, ok := repByEndUser[eu]; !ok {
			repByEndUser[eu] = id
			defaultPicked[eu] = row.IsDefault
		} else if row.IsDefault && !defaultPicked[eu] {
			repByEndUser[eu] = id
			defaultPicked[eu] = true
		}
		if _, ok := nameByEndUser[eu]; !ok {
			if label := DisplayNameForEndUser(eu); label != "" {
				nameByEndUser[eu] = label
			} else if n := strings.TrimSpace(row.Name); n != "" {
				nameByEndUser[eu] = n
			}
		}
	}
	return repByEndUser, nameByEndUser
}

func buildSingleAPIKeySelectorClause(selector string) (string, []interface{}) {
	return buildSingleAPIKeySelectorClauseForTenant(systemTenantID, selector)
}

func buildSingleAPIKeySelectorClauseForTenant(tenantID, selector string) (string, []interface{}) {
	trimmed := strings.TrimSpace(selector)
	if trimmed == "" {
		return "", nil
	}
	tenantID = normalizeTenantID(tenantID)
	row := GetAPIKeyForTenant(tenantID, trimmed)
	if row == nil {
		// Secret may resolve outside the tenant pin (legacy global lookup).
		row = GetAPIKey(trimmed)
	}
	// Always key-scoped. Account-wide views use EndUserID on LogQueryParams, not secret expansion.
	if row != nil {
		if id := strings.TrimSpace(row.ID); id != "" {
			return " WHERE api_key_id = ?", []interface{}{id}
		}
	}
	// Not a stored key. A route or internal origin is stored verbatim and can be
	// matched as such. Anything else is a secret the system no longer knows: its
	// rows hold a mask, which says nothing about which secret it was, so it must
	// match nothing instead of falling back to a column compare.
	if IsRequestLogLabel(trimmed) {
		return " WHERE api_key = ?", []interface{}{trimmed}
	}
	return " WHERE 1 = 0", nil
}

// buildPublicLookupAPIKeySelectorClause matches any key in the end-user account pool
// (or a single standalone key). Used by public log content access checks.
func buildPublicLookupAPIKeySelectorClause(tenantID, selector string) (string, []interface{}) {
	keys := ExpandPublicLookupAPIKeys(selector)
	if len(keys) == 0 {
		return " WHERE 1 = 0", nil
	}
	if len(keys) == 1 {
		return buildSingleAPIKeySelectorClauseForTenant(tenantID, keys[0])
	}
	conds := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*2)
	for _, k := range keys {
		clause, clauseArgs := buildSingleAPIKeySelectorClauseForTenant(tenantID, k)
		conds = append(conds, strings.TrimPrefix(clause, " WHERE "))
		args = append(args, clauseArgs...)
	}
	return " WHERE (" + strings.Join(conds, " OR ") + ")", args
}

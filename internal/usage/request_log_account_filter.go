package usage

import (
	"database/sql"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// queryDistinctAPIKeys returns one filter option per account (owned multi-key
// end users collapse onto a representative secret + display name). Standalone
// keys stay one option each so "测试 key" never sits next to "Kittors".
func queryDistinctAPIKeys(db *sql.DB, params LogQueryParams) ([]string, map[string]string, map[string]int64, error) {
	tenantID := normalizeTenantID(params.TenantID)
	currentByID := currentAPIKeyRowsByIDForTenant(tenantID)
	currentByKey := currentAPIKeyRowsByKeyForTenant(tenantID)
	repByEndUser, nameByEndUser := accountFilterRepresentatives(tenantID)
	where, args := buildWhereClause(params)
	if where == "" {
		where = " WHERE api_key != ''"
	} else {
		where += " AND api_key != ''"
	}
	rows, err := db.Query(`
		SELECT
			CASE
				WHEN trim(coalesce(api_key_id, '')) <> '' THEN api_key_id
				ELSE 'raw:' || api_key
			END AS logical_selector,
			COALESCE(MAX(NULLIF(trim(coalesce(api_key_id, '')), '')), '') AS logical_id,
			MAX(api_key) AS snapshot_key,
			COALESCE(NULLIF(MAX(api_key_name), ''), '') AS snapshot_name,
			COUNT(*) AS request_count
		FROM request_logs
		`+where+`
		GROUP BY logical_selector
		ORDER BY logical_selector
	`, args...)
	if err != nil {
		log.Warnf("usage: distinct api_key logical groups query failed: %v", err)
		return nil, nil, nil, fmt.Errorf("usage: distinct api_key logical groups: %w", err)
	}
	defer rows.Close()

	values := make([]string, 0)
	names := make(map[string]string)
	counts := make(map[string]int64)
	seenAccount := make(map[string]struct{})
	for rows.Next() {
		var logicalSelector string
		var logicalID sql.NullString
		var snapshotKey string
		var snapshotName string
		var requestCount int64
		if err := rows.Scan(&logicalSelector, &logicalID, &snapshotKey, &snapshotName, &requestCount); err != nil {
			log.Warnf("usage: distinct api_key scan failed: %v", err)
			return nil, nil, nil, err
		}

		value := strings.TrimSpace(snapshotKey)
		name := strings.TrimSpace(snapshotName)
		var row *APIKeyRow
		if r, ok := currentByID[trimNullString(logicalID)]; ok {
			copy := r
			row = &copy
		} else if r, ok := currentByKey[value]; ok {
			copy := r
			row = &copy
		}
		accountID := ""
		if row != nil {
			if trimmed := strings.TrimSpace(row.Key); trimmed != "" {
				value = trimmed
			}
			if eu := strings.TrimSpace(row.EndUserID); eu != "" {
				accountID = eu
				if rep := strings.TrimSpace(repByEndUser[eu]); rep != "" {
					value = rep
				}
				if label := strings.TrimSpace(nameByEndUser[eu]); label != "" {
					name = label
				} else if label := ResolveAPIKeyDisplayName(row, name); label != "" {
					name = label
				}
			} else if label := ResolveAPIKeyDisplayName(row, name); label != "" {
				name = label
			}
		}
		if value == "" {
			continue
		}
		// Dedupe: end-user account id when owned, else raw key secret.
		dedupeKey := value
		if accountID != "" {
			dedupeKey = "eu:" + accountID
		}
		// Legacy raw-key and stable-id groups can resolve to the same account.
		// Sum before deduping so the UI count matches every retained log for that account.
		counts[value] += requestCount
		if _, ok := seenAccount[dedupeKey]; ok {
			if name != "" && names[value] == "" {
				names[value] = name
			}
			continue
		}
		seenAccount[dedupeKey] = struct{}{}
		values = append(values, value)
		if name != "" {
			names[value] = name
		}
	}
	return values, names, counts, rows.Err()
}

// queryDistinctAPIKeyIDs returns one public-safe filter option per concrete key
// (stable api_key_id + own name). Unlike queryDistinctAPIKeys it does not collapse
// multi-key end-user accounts onto a representative secret.
func queryDistinctAPIKeyIDs(db *sql.DB, params LogQueryParams) ([]string, map[string]string, map[string]int64, error) {
	tenantID := normalizeTenantID(params.TenantID)
	currentByID := currentAPIKeyRowsByIDForTenant(tenantID)
	currentByKey := currentAPIKeyRowsByKeyForTenant(tenantID)
	where, args := buildWhereClause(params)
	if where == "" {
		where = " WHERE api_key != ''"
	} else {
		where += " AND api_key != ''"
	}
	rows, err := db.Query(`
		SELECT
			CASE
				WHEN trim(coalesce(api_key_id, '')) <> '' THEN api_key_id
				ELSE 'raw:' || api_key
			END AS logical_selector,
			COALESCE(MAX(NULLIF(trim(coalesce(api_key_id, '')), '')), '') AS logical_id,
			MAX(api_key) AS snapshot_key,
			COALESCE(NULLIF(MAX(api_key_name), ''), '') AS snapshot_name,
			COUNT(*) AS request_count
		FROM request_logs
		`+where+`
		GROUP BY logical_selector
		ORDER BY logical_selector
	`, args...)
	if err != nil {
		log.Warnf("usage: distinct api_key_id logical groups query failed: %v", err)
		return nil, nil, nil, fmt.Errorf("usage: distinct api_key_id logical groups: %w", err)
	}
	defer rows.Close()

	values := make([]string, 0)
	names := make(map[string]string)
	counts := make(map[string]int64)
	seen := make(map[string]struct{})
	for rows.Next() {
		var logicalSelector string
		var logicalID sql.NullString
		var snapshotKey string
		var snapshotName string
		var requestCount int64
		if err := rows.Scan(&logicalSelector, &logicalID, &snapshotKey, &snapshotName, &requestCount); err != nil {
			log.Warnf("usage: distinct api_key_id scan failed: %v", err)
			return nil, nil, nil, err
		}

		id := strings.TrimSpace(trimNullString(logicalID))
		value := strings.TrimSpace(snapshotKey)
		name := strings.TrimSpace(snapshotName)
		var row *APIKeyRow
		if id != "" {
			if r, ok := currentByID[id]; ok {
				copy := r
				row = &copy
			}
		}
		if row == nil {
			if r, ok := currentByKey[value]; ok {
				copy := r
				row = &copy
			}
		}
		if row != nil {
			if trimmed := strings.TrimSpace(row.ID); trimmed != "" {
				id = trimmed
			}
			if own := strings.TrimSpace(row.Name); own != "" {
				name = own
			}
		}
		// Public filter values must be stable ids, never secrets.
		if id == "" {
			continue
		}
		// Stable-id and legacy raw-key groups can map to the same current key.
		counts[id] += requestCount
		if _, ok := seen[id]; ok {
			if name != "" && names[id] == "" {
				names[id] = name
			}
			continue
		}
		seen[id] = struct{}{}
		values = append(values, id)
		if name != "" {
			names[id] = name
		}
	}
	return values, names, counts, rows.Err()
}

// accountFilterRepresentatives picks one filter value per end-user (prefer default key).
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
		key := strings.TrimSpace(row.Key)
		if key == "" {
			continue
		}
		if _, ok := repByEndUser[eu]; !ok {
			repByEndUser[eu] = key
			defaultPicked[eu] = row.IsDefault
		} else if row.IsDefault && !defaultPicked[eu] {
			repByEndUser[eu] = key
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
	// Owned keys: selecting the account filter matches the whole key pool.
	if row != nil {
		if eu := strings.TrimSpace(row.EndUserID); eu != "" {
			pred, args := buildEndUserAPIKeySelectorPredicate(tenantID, eu)
			return " WHERE " + pred, args
		}
		if id := strings.TrimSpace(row.ID); id != "" {
			return " WHERE (api_key_id = ? OR (api_key_id = '' AND api_key = ?))", []interface{}{id, strings.TrimSpace(row.Key)}
		}
	}
	return " WHERE api_key = ?", []interface{}{trimmed}
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

package usage

import "strings"

// apiKeyFilterConditions builds the key-identity predicates of a log query: the
// stable key-id filter first, then the legacy multi-value api_key filter. The
// result is appended to buildWhereClause's conditions in the same position the
// blocks used to occupy inline.
func apiKeyFilterConditions(params LogQueryParams) ([]string, []interface{}) {
	conds := make([]string, 0, 2)
	args := make([]interface{}, 0, 4)

	// Stable key-id multi-value filter (public key facet; does not expand account pools).
	if len(params.APIKeyIDs) > 0 {
		idConds := make([]string, 0, len(params.APIKeyIDs))
		for _, id := range params.APIKeyIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			row := GetAPIKeyByIDForTenant(params.TenantID, id)
			if row == nil {
				row = GetAPIKeyByID(id)
			}
			if row != nil {
				secret := strings.TrimSpace(row.Key)
				if secret != "" {
					idConds = append(idConds, "(api_key_id = ? OR (trim(coalesce(api_key_id, '')) = '' AND api_key = ?))")
					args = append(args, id, secret)
					continue
				}
			}
			idConds = append(idConds, "api_key_id = ?")
			args = append(args, id)
		}
		if len(idConds) == 1 {
			conds = append(conds, idConds[0])
		} else if len(idConds) > 1 {
			conds = append(conds, "("+strings.Join(idConds, " OR ")+")")
		}
	}

	// API Key multi-value filter
	if len(params.APIKeys) > 0 {
		apiKeyConds := make([]string, 0, len(params.APIKeys))
		systemConds := make([]string, 0)
		normalKeys := make([]string, 0, len(params.APIKeys))
		for _, key := range params.APIKeys {
			if key == systemRequestLogFilterValue {
				systemConds = append(systemConds, `(
					trim(coalesce(api_key_name, '')) = ''
					AND (
						trim(coalesce(api_key, '')) = ''
						OR trim(coalesce(api_key, '')) LIKE '/%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'GET /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'POST /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'PUT /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'PATCH /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'DELETE /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'OPTIONS /%'
						OR upper(trim(coalesce(api_key, ''))) LIKE 'HEAD /%'
					)
				)`)
			} else {
				normalKeys = append(normalKeys, key)
			}
		}
		if len(normalKeys) > 0 {
			for _, k := range normalKeys {
				clause, clauseArgs := buildSingleAPIKeySelectorClauseForTenant(params.TenantID, k)
				apiKeyConds = append(apiKeyConds, strings.TrimPrefix(clause, " WHERE "))
				args = append(args, clauseArgs...)
			}
		}
		apiKeyConds = append(apiKeyConds, systemConds...)
		if len(apiKeyConds) == 1 {
			conds = append(conds, apiKeyConds[0])
		} else if len(apiKeyConds) > 1 {
			conds = append(conds, "("+strings.Join(apiKeyConds, " OR ")+")")
		}
	}

	return conds, args
}

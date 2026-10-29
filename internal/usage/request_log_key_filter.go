package usage

import (
	"sort"
	"strings"
)

// SystemRequestLogFilterValue is the key-filter value that stands for system
// calls: requests that carried no client key. It appears in the key facet and is
// accepted wherever a key id is.
const SystemRequestLogFilterValue = "__system__"

// systemRequestLogPredicate selects the "system call" rows: work that carried no
// client key, so its api_key is empty, a route ("POST /auth-files/warmup") or an
// internal origin ("warmup"). A named key never counts as a system call. The
// panel groups rows by the same rule when it renders the key column, which is
// why the label set comes from IsRequestLogLabel's own tables.
func systemRequestLogPredicate() string {
	var b strings.Builder
	b.WriteString("(trim(coalesce(api_key_name, '')) = '' AND (trim(coalesce(api_key, '')) = ''")
	b.WriteString(" OR trim(coalesce(api_key, '')) LIKE '/%'")
	for _, method := range requestLogRouteMethods {
		b.WriteString(" OR upper(trim(coalesce(api_key, ''))) LIKE '" + method + " /%'")
	}
	labels := make([]string, 0, len(requestLogInternalLabels))
	for label := range requestLogInternalLabels {
		labels = append(labels, "'"+label+"'")
	}
	sort.Strings(labels)
	b.WriteString(" OR lower(trim(coalesce(api_key, ''))) IN (" + strings.Join(labels, ", ") + ")")
	b.WriteString("))")
	return b.String()
}

// apiKeyFilterConditions builds the key-identity predicates of a log query: the
// stable key-id filter first, then the multi-value api_key selector filter.
// The result is appended to buildWhereClause's conditions in the same position
// the blocks used to occupy inline.
//
// A row belongs to a key through request_logs.api_key_id only. The api_key
// column holds a mask (or a route label), never a secret to compare against.
func apiKeyFilterConditions(params LogQueryParams) ([]string, []interface{}) {
	conds := make([]string, 0, 2)
	args := make([]interface{}, 0, 4)

	// Stable key-id multi-value filter (does not expand account pools). The
	// "__system__" entry stands for the system-call rows, which have no key id.
	if len(params.APIKeyIDs) > 0 {
		idConds := make([]string, 0, 2)
		ids := make([]string, 0, len(params.APIKeyIDs))
		includeSystem := false
		for _, id := range params.APIKeyIDs {
			id = strings.TrimSpace(id)
			switch id {
			case "":
			case SystemRequestLogFilterValue:
				includeSystem = true
			default:
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
			idConds = append(idConds, "api_key_id IN ("+placeholders+")")
			for _, id := range ids {
				args = append(args, id)
			}
		}
		if includeSystem {
			idConds = append(idConds, systemRequestLogPredicate())
		}
		if len(idConds) == 1 {
			conds = append(conds, idConds[0])
		} else if len(idConds) > 1 {
			conds = append(conds, "("+strings.Join(idConds, " OR ")+")")
		}
	}

	// API Key selector filter. A selector is something the caller already holds
	// (a secret they present, the "__system__" sentinel); it is resolved to the
	// key's id here, so the secret itself never reaches the SQL.
	if len(params.APIKeys) > 0 {
		apiKeyConds := make([]string, 0, len(params.APIKeys))
		systemConds := make([]string, 0)
		normalKeys := make([]string, 0, len(params.APIKeys))
		for _, key := range params.APIKeys {
			if key == SystemRequestLogFilterValue {
				systemConds = append(systemConds, systemRequestLogPredicate())
			} else {
				normalKeys = append(normalKeys, key)
			}
		}
		for _, k := range normalKeys {
			clause, clauseArgs := buildSingleAPIKeySelectorClauseForTenant(params.TenantID, k)
			apiKeyConds = append(apiKeyConds, strings.TrimPrefix(clause, " WHERE "))
			args = append(args, clauseArgs...)
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

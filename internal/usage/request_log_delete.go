package usage

import (
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// DeleteLogsByAPIKey removes all request_logs and request_log_content entries
// for the given API key. Returns the number of deleted log rows.
func DeleteLogsByAPIKey(apiKey string) (int64, error) {
	return DeleteLogsByAPIKeyForTenant(systemTenantID, apiKey)
}

func DeleteLogsByAPIKeyForTenant(tenantID, apiKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, fmt.Errorf("usage: database not initialised")
	}
	if apiKey == "" {
		return 0, fmt.Errorf("usage: empty api_key")
	}

	// Delete associated content rows first (FK cascade may handle this,
	// but be explicit to ensure cleanup even without FK enforcement).
	tenantID = normalizeTenantID(tenantID)
	clause, args := buildSingleAPIKeySelectorClauseForTenant(tenantID, apiKey)
	predicate := strings.TrimPrefix(clause, " WHERE ")
	args = append([]interface{}{tenantID}, args...)
	clause = " WHERE tenant_id = ? AND " + predicate
	_, _ = db.Exec(
		`DELETE FROM request_log_content WHERE tenant_id = ? AND log_id IN
		 (SELECT id FROM request_logs`+clause+`)`, append([]interface{}{tenantID}, args...)...)

	result, err := db.Exec("DELETE FROM request_logs"+clause, args...)
	if err != nil {
		return 0, fmt.Errorf("usage: delete logs by api_key: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows: %w", err)
	}
	if deleted > 0 {
		log.Infof("usage: deleted %d request log(s) for api_key=%s", deleted, apiKey)
	}
	return deleted, nil
}

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

// DeleteLogsByAPIKeyForTenant deletes the history of the key that apiKey (a
// secret the caller holds) resolves to. It must run while the key still exists:
// once the key is gone its secret resolves to nothing, and the stored rows hold
// only a mask. Callers that delete the key itself use DeleteLogsByAPIKeyIDForTenant.
func DeleteLogsByAPIKeyForTenant(tenantID, apiKey string) (int64, error) {
	if apiKey == "" {
		return 0, fmt.Errorf("usage: empty api_key")
	}
	tenantID = normalizeTenantID(tenantID)
	clause, args := buildSingleAPIKeySelectorClauseForTenant(tenantID, apiKey)
	return deleteRequestLogs(tenantID, clause, args, "api_key="+MaskAPIKey(apiKey))
}

// DeleteLogsByAPIKeyIDForTenant deletes the history of the key with the given
// stable id. This is how the history of a deleted key is removed: the id outlives
// the key, the secret does not.
func DeleteLogsByAPIKeyIDForTenant(tenantID, apiKeyID string) (int64, error) {
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		return 0, fmt.Errorf("usage: empty api_key_id")
	}
	return deleteRequestLogs(normalizeTenantID(tenantID), " WHERE api_key_id = ?", []interface{}{apiKeyID}, "api_key_id="+apiKeyID)
}

// deleteRequestLogs removes the rows selected by clause (a key predicate) and
// their content. what names the key for the log line and must not be a secret.
func deleteRequestLogs(tenantID, clause string, args []interface{}, what string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, fmt.Errorf("usage: database not initialised")
	}

	// Delete associated content rows first (FK cascade may handle this,
	// but be explicit to ensure cleanup even without FK enforcement).
	predicate := strings.TrimPrefix(clause, " WHERE ")
	args = append([]interface{}{tenantID}, args...)
	clause = " WHERE tenant_id = ? AND " + predicate
	_, _ = db.Exec(
		`DELETE FROM request_log_content WHERE tenant_id = ? AND log_id IN
		 (SELECT id FROM request_logs`+clause+`)`, append([]interface{}{tenantID}, args...)...)

	result, err := db.Exec("DELETE FROM request_logs"+clause, args...)
	if err != nil {
		return 0, fmt.Errorf("usage: delete logs by api key: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("usage: affected rows: %w", err)
	}
	if deleted > 0 {
		log.Infof("usage: deleted %d request log(s) for %s", deleted, what)
	}
	return deleted, nil
}

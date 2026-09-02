package usage

import (
	"context"
	"database/sql"
	"strings"

	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	log "github.com/sirupsen/logrus"
)

type APIKeyIdentity struct {
	ID   string
	Key  string
	Name string
}

// BackfillLegacyRequestLogsAPIKeyIDForTenant preserves account ownership for
// legacy rows that only recorded the raw secret before an owned key is rotated.
func BackfillLegacyRequestLogsAPIKeyIDForTenant(ctx context.Context, tenantID, apiKeyID, oldSecret string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	tenantID = normalizeTenantID(tenantID)
	apiKeyID = strings.TrimSpace(apiKeyID)
	oldSecret = strings.TrimSpace(oldSecret)
	if apiKeyID == "" || oldSecret == "" {
		return 0, nil
	}
	result, err := db.ExecContext(ctx, `
		UPDATE request_logs
		SET api_key_id = ?
		WHERE tenant_id = ?
		  AND trim(coalesce(api_key_id, '')) = ''
		  AND api_key = ?
	`, apiKeyID, tenantID, oldSecret)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func ResolveAPIKeyIdentity(key string) *APIKeyIdentity {
	row := GetAPIKey(strings.TrimSpace(key))
	if row == nil || strings.TrimSpace(row.ID) == "" {
		return nil
	}
	return &APIKeyIdentity{
		ID:   strings.TrimSpace(row.ID),
		Key:  strings.TrimSpace(row.Key),
		Name: strings.TrimSpace(row.Name),
	}
}

func currentAPIKeyRowsByID() map[string]APIKeyRow {
	rows := ListAPIKeys()
	result := make(map[string]APIKeyRow, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(row.ID)
		if id == "" {
			continue
		}
		result[id] = row
	}
	return result
}

func currentAPIKeyRowsByIDForTenant(tenantID string) map[string]APIKeyRow {
	rows := ListAPIKeysForTenant(tenantID)
	result := make(map[string]APIKeyRow, len(rows))
	for _, row := range rows {
		id := strings.TrimSpace(row.ID)
		if id != "" {
			result[id] = row
		}
	}
	return result
}

// currentAPIKeyRowsByKeyForTenant indexes live API keys by raw secret so
// legacy request_logs rows missing api_key_id can still resolve to the same
// identity as rows that already carry the stable id.
func currentAPIKeyRowsByKeyForTenant(tenantID string) map[string]APIKeyRow {
	rows := ListAPIKeysForTenant(tenantID)
	result := make(map[string]APIKeyRow, len(rows))
	for _, row := range rows {
		key := strings.TrimSpace(row.Key)
		if key != "" {
			result[key] = row
		}
	}
	return result
}

func uniqueAPIKeyIDByName() map[string]string {
	return uniqueAPIKeyIDByNameFromRows(ListAPIKeys())
}

func uniqueAPIKeyIDByNameFromDB(db *sql.DB) map[string]string {
	if db == nil {
		return nil
	}
	return uniqueAPIKeyIDByNameFromRows(sqlapikey.NewStore(db).List())
}

func uniqueAPIKeyIDByNameFromRows(rows []APIKeyRow) map[string]string {
	counts := make(map[string]int)
	ids := make(map[string]string)
	for _, row := range rows {
		id := strings.TrimSpace(row.ID)
		name := strings.ToLower(strings.TrimSpace(row.Name))
		if id == "" || name == "" {
			continue
		}
		counts[name]++
		ids[name] = id
	}

	result := make(map[string]string)
	for name, id := range ids {
		if counts[name] == 1 {
			result[name] = id
		}
	}
	return result
}

func uniqueRequestLogAPIKeyIDByKeyFromDB(db *sql.DB) map[string]string {
	if db == nil {
		return nil
	}

	rows, err := db.Query(`
		SELECT existing.api_key, existing.api_key_id
		FROM request_logs AS existing
		JOIN (
			SELECT DISTINCT api_key
			FROM request_logs
			WHERE api_key_id = ''
			  AND api_key != ''
		) AS missing ON missing.api_key = existing.api_key
		WHERE existing.api_key_id != ''
	`)
	if err != nil {
		log.Warnf("usage: query unique request_log api_key_id by raw key failed: %v", err)
		return nil
	}
	defer rows.Close()

	conflicts := make(map[string]bool)
	ids := make(map[string]string)
	for rows.Next() {
		var rawKey string
		var rawID string
		if err := rows.Scan(&rawKey, &rawID); err != nil {
			log.Warnf("usage: scan request_log api_key identity row failed: %v", err)
			return nil
		}
		key := strings.TrimSpace(rawKey)
		id := strings.TrimSpace(rawID)
		if key == "" || id == "" {
			continue
		}
		if existing, ok := ids[key]; ok {
			if existing != id {
				conflicts[key] = true
				continue
			}
		} else {
			ids[key] = id
		}
	}

	result := make(map[string]string)
	for key, id := range ids {
		if !conflicts[key] {
			result[key] = id
		}
	}
	return result
}

func backfillRequestLogAPIKeyIDs(db *sql.DB) {
	if db == nil {
		return
	}
	if !hasRequestLogsMissingAPIKeyID(db) {
		return
	}

	result, err := db.Exec(`
		UPDATE request_logs
		SET api_key_id = (
			SELECT id FROM api_keys WHERE api_keys.key = request_logs.api_key
		)
		WHERE api_key_id = ''
		  AND api_key != ''
		  AND EXISTS (
			SELECT 1
			FROM api_keys
			WHERE api_keys.key = request_logs.api_key
			  AND api_keys.id != ''
		  )
	`)
	if err != nil {
		log.Warnf("usage: backfill request_logs api_key_id by key failed: %v", err)
	} else if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
		log.Infof("usage: backfilled api_key_id for %d request_logs by exact key match", rows)
	}
	if !hasRequestLogsMissingAPIKeyID(db) {
		return
	}

	nameToID := uniqueAPIKeyIDByNameFromDB(db)
	if len(nameToID) == 0 {
		return
	}
	for lowerName, id := range nameToID {
		result, err := db.Exec(`
			UPDATE request_logs
			SET api_key_id = ?
			WHERE api_key_id = ''
			  AND lower(trim(coalesce(api_key_name, ''))) = ?
			  AND api_key != ''
		`, id, lowerName)
		if err != nil {
			log.Warnf("usage: backfill request_logs api_key_id by name failed for %q: %v", lowerName, err)
			continue
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			log.Infof("usage: backfilled api_key_id for %d request_logs by unique api_key_name=%q", rows, lowerName)
		}
	}
	if !hasRequestLogsMissingAPIKeyID(db) {
		return
	}

	keyToID := uniqueRequestLogAPIKeyIDByKeyFromDB(db)
	if len(keyToID) == 0 {
		return
	}
	for rawKey, id := range keyToID {
		result, err := db.Exec(`
			UPDATE request_logs
			SET api_key_id = ?
			WHERE api_key_id = ''
			  AND api_key = ?
		`, id, rawKey)
		if err != nil {
			log.Warnf("usage: backfill request_logs api_key_id by historical raw key failed for %q: %v", rawKey, err)
			continue
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil && rows > 0 {
			log.Infof("usage: backfilled api_key_id for %d request_logs by historical raw api_key=%q", rows, rawKey)
		}
	}
}

func hasRequestLogsMissingAPIKeyID(db *sql.DB) bool {
	var exists int
	err := db.QueryRow("SELECT 1 FROM request_logs WHERE api_key_id = '' AND api_key != '' LIMIT 1").Scan(&exists)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		log.Warnf("usage: query request_logs missing api_key_id failed: %v", err)
		return false
	}
	return exists == 1
}

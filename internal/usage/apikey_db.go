package usage

import (
	"database/sql"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlite/apikey"
	log "github.com/sirupsen/logrus"
)

// Compatibility bridge contract:
// - Owner: API key settings / config-access boundary.
// - Real implementation: internal/storage/sqlite/apikey and internal/management/settings/apikey.
// - Allowed callers: legacy adapters still being migrated; new management/config-access code should use apikey service first.
// - Exit condition: remaining callers move to apikey service or sqlite store bridge; do not add new imports here.
type APIKeyRow = sqlapikey.APIKeyRow

func APIKeyRowFromConfig(entry config.APIKeyEntry) APIKeyRow {
	return sqlapikey.APIKeyRowFromConfig(entry)
}

func initAPIKeysTable(db *sql.DB) {
	sqlapikey.InitTable(db)
}

func apiKeyStore() sqlapikey.Store {
	return sqlapikey.NewStore(getDB())
}

func apiKeyStoreForTenant(tenantID string) sqlapikey.Store {
	return sqlapikey.NewTenantStore(getDB(), tenantID)
}

func defaultAPIKeyName(index int) string {
	return sqlapikey.DefaultAPIKeyName(index)
}

func backfillAPIKeyNames(db *sql.DB) {
	sqlapikey.BackfillNames(db)
}

// MigrateAPIKeysFromConfig moves API key entries from YAML config into SQLite.
// It only migrates if the api_keys table is empty AND the config has entries.
// After migration, it backs up config.yaml and re-saves it without the API key
// fields so the YAML file stays clean.
// Fail-closed: DB query/write errors return err so callers do not mark one-shot
// end-user backfill complete against an incomplete key set.
func MigrateAPIKeysFromConfig(cfg *config.Config, configFilePath string) (int, error) {
	db := getDB()
	if db == nil || cfg == nil {
		return 0, nil
	}

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 {
		cfg.APIKeys = nil
		cfg.APIKeyEntries = nil
		if configFilePath != "" {
			cleanAPIKeysFromYAML(configFilePath)
		}
		return 0, nil
	}

	seen := make(map[string]struct{})
	rows := make([]APIKeyRow, 0)
	now := time.Now().UTC().Format(time.RFC3339)

	for _, entry := range cfg.APIKeyEntries {
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		row := APIKeyRowFromConfig(entry)
		row.Key = trimmed
		row.Name = strings.TrimSpace(row.Name)
		if row.Name == "" {
			row.Name = sqlapikey.DefaultAPIKeyName(len(rows))
		}
		if row.CreatedAt == "" {
			row.CreatedAt = now
		}
		row.UpdatedAt = now
		rows = append(rows, row)
	}

	for _, key := range cfg.APIKeys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		rows = append(rows, APIKeyRow{
			Key:       trimmed,
			Name:      sqlapikey.DefaultAPIKeyName(len(rows)),
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	if len(rows) == 0 {
		return 0, nil
	}

	if err := apiKeyStore().ReplaceAll(rows); err != nil {
		return 0, err
	}

	log.Infof("usage: migrated %d API keys from config to SQLite", len(rows))

	cfg.APIKeys = nil
	cfg.APIKeyEntries = nil

	if configFilePath != "" {
		if backupConfigForMigration(configFilePath, apiKeysMigrationBackupSuffix) {
			cleanAPIKeysFromYAML(configFilePath)
		}
	}

	return len(rows), nil
}

// EffectiveAPIKeyRow applies the currently linked permission profile to an API key row.
// If the profile is missing, the row's copied/custom settings remain the fallback.
func EffectiveAPIKeyRow(row APIKeyRow) APIKeyRow {
	return EffectiveAPIKeyRowForTenant(row.TenantID, row)
}

func EffectiveAPIKeyRowForTenant(tenantID string, row APIKeyRow) APIKeyRow {
	return EffectiveAPIKeyRowWithProfiles(row, ListAPIKeyPermissionProfilesForTenant(tenantID))
}

// EffectiveAPIKeyRowWithProfiles applies a preloaded permission profile snapshot.
func EffectiveAPIKeyRowWithProfiles(row APIKeyRow, profiles []APIKeyPermissionProfileRow) APIKeyRow {
	return sqlapikey.EffectiveAPIKeyRowWithProfiles(row, toPermissionProfileSnapshots(profiles))
}

// EffectiveAPIKeyRows applies the current permission profile snapshot to each row.
func EffectiveAPIKeyRows(rows []APIKeyRow) []APIKeyRow {
	return EffectiveAPIKeyRowsForTenant("", rows)
}

func EffectiveAPIKeyRowsForTenant(tenantID string, rows []APIKeyRow) []APIKeyRow {
	if len(rows) == 0 {
		return rows
	}
	return sqlapikey.EffectiveAPIKeyRowsWithProfiles(rows, toPermissionProfileSnapshots(ListAPIKeyPermissionProfilesForTenant(tenantID)))
}

// ListAPIKeys retrieves system-tenant API key entries for legacy callers.
func ListAPIKeys() []APIKeyRow {
	return apiKeyStore().List()
}

func ListAPIKeysForTenant(tenantID string) []APIKeyRow {
	return apiKeyStoreForTenant(tenantID).List()
}

func ListAllAPIKeys() []APIKeyRow {
	return apiKeyStore().ListAll()
}

// GetAPIKey retrieves a single API key entry by key string.
func GetAPIKey(key string) *APIKeyRow {
	return apiKeyStore().Get(key)
}

// GetAPIKeyByID retrieves a single API key entry by stable id.
func GetAPIKeyByID(id string) *APIKeyRow {
	return apiKeyStore().GetByID(id)
}

// UpsertAPIKey inserts or updates an API key entry.
func UpsertAPIKey(entry APIKeyRow) error {
	return apiKeyStore().Upsert(entry)
}

// UpdateAPIKeyByID updates an API key entry by stable id.
func UpdateAPIKeyByID(entry APIKeyRow) error {
	return apiKeyStore().UpdateByID(entry)
}

// DeleteAPIKey removes an API key entry by key string.
func DeleteAPIKey(key string) error {
	return apiKeyStore().Delete(key)
}

// DeleteAPIKeyByID removes an API key entry by stable id.
func DeleteAPIKeyByID(id string) error {
	return apiKeyStore().DeleteByID(id)
}

// ReplaceAllAPIKeys atomically replaces all API keys with the given list.
func ReplaceAllAPIKeys(entries []APIKeyRow) error {
	return apiKeyStore().ReplaceAll(entries)
}

func GetAPIKeyForTenant(tenantID, key string) *APIKeyRow {
	return apiKeyStoreForTenant(tenantID).Get(key)
}

func GetAPIKeyByIDForTenant(tenantID, id string) *APIKeyRow {
	return apiKeyStoreForTenant(tenantID).GetByID(id)
}

func UpsertAPIKeyForTenant(tenantID string, entry APIKeyRow) error {
	entry.TenantID = tenantID
	return apiKeyStoreForTenant(tenantID).Upsert(entry)
}

func UpdateAPIKeyByIDForTenant(tenantID string, entry APIKeyRow) error {
	entry.TenantID = tenantID
	return apiKeyStoreForTenant(tenantID).UpdateByID(entry)
}

func DeleteAPIKeyForTenant(tenantID, key string) error {
	return apiKeyStoreForTenant(tenantID).Delete(key)
}

func DeleteAPIKeyByIDForTenant(tenantID, id string) error {
	return apiKeyStoreForTenant(tenantID).DeleteByID(id)
}

func ReplaceAllAPIKeysForTenant(tenantID string, entries []APIKeyRow) error {
	return apiKeyStoreForTenant(tenantID).ReplaceAll(entries)
}

func toPermissionProfileSnapshots(profiles []APIKeyPermissionProfileRow) []sqlapikey.PermissionProfileSnapshot {
	if len(profiles) == 0 {
		return nil
	}

	snapshots := make([]sqlapikey.PermissionProfileSnapshot, 0, len(profiles))
	for _, profile := range profiles {
		snapshots = append(snapshots, sqlapikey.PermissionProfileSnapshot{
			ID:                   profile.ID,
			DailyLimit:           profile.DailyLimit,
			TotalQuota:           profile.TotalQuota,
			DailySpendingLimit:   profile.DailySpendingLimit,
			ConcurrencyLimit:     profile.ConcurrencyLimit,
			RPMLimit:             profile.RPMLimit,
			TPMLimit:             profile.TPMLimit,
			AllowedModels:        append([]string(nil), profile.AllowedModels...),
			AllowedChannels:      append([]string(nil), profile.AllowedChannels...),
			AllowedChannelGroups: append([]string(nil), profile.AllowedChannelGroups...),
			SystemPrompt:         profile.SystemPrompt,
		})
	}
	return snapshots
}

// ResolveAPIKeyTenant returns the trusted tenant scope attached to an API key.
// Legacy rows are assigned to the built-in system tenant by the PostgreSQL migration.
func ResolveAPIKeyTenant(key string) string {
	db := getReadDB()
	if db == nil {
		return ""
	}
	var tenantID string
	if err := db.QueryRow(`SELECT tenant_id FROM api_keys WHERE key = ?`, strings.TrimSpace(key)).Scan(&tenantID); err != nil {
		return ""
	}
	return strings.TrimSpace(tenantID)
}

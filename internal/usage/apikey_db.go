package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

// APIKeyRow mirrors config.APIKeyEntry and is used for SQLite persistence.
type APIKeyRow struct {
	Key                  string   `json:"key"`
	Name                 string   `json:"name,omitempty"`
	Disabled             bool     `json:"disabled,omitempty"`
	DailyLimit           int      `json:"daily-limit,omitempty"`
	TotalQuota           int      `json:"total-quota,omitempty"`
	SpendingLimit        float64  `json:"spending-limit,omitempty"`
	ConcurrencyLimit     int      `json:"concurrency-limit,omitempty"`
	RPMLimit             int      `json:"rpm-limit,omitempty"`
	TPMLimit             int      `json:"tpm-limit,omitempty"`
	AllowedModels        []string `json:"allowed-models,omitempty"`
	AllowedChannels      []string `json:"allowed-channels,omitempty"`
	AllowedChannelGroups []string `json:"allowed-channel-groups,omitempty"`
	SystemPrompt         string   `json:"system-prompt,omitempty"`
	CreatedAt            string   `json:"created-at,omitempty"`
	UpdatedAt            string   `json:"updated-at,omitempty"`
}

// ToConfigEntry converts an APIKeyRow to a config.APIKeyEntry.
func (r *APIKeyRow) ToConfigEntry() config.APIKeyEntry {
	return config.APIKeyEntry{
		Key:                  r.Key,
		Name:                 r.Name,
		Disabled:             r.Disabled,
		DailyLimit:           r.DailyLimit,
		TotalQuota:           r.TotalQuota,
		SpendingLimit:        r.SpendingLimit,
		ConcurrencyLimit:     r.ConcurrencyLimit,
		RPMLimit:             r.RPMLimit,
		TPMLimit:             r.TPMLimit,
		AllowedModels:        r.AllowedModels,
		AllowedChannels:      r.AllowedChannels,
		AllowedChannelGroups: r.AllowedChannelGroups,
		SystemPrompt:         r.SystemPrompt,
		CreatedAt:            r.CreatedAt,
	}
}

// APIKeyRowFromConfig converts a config.APIKeyEntry to an APIKeyRow.
func APIKeyRowFromConfig(entry config.APIKeyEntry) APIKeyRow {
	return APIKeyRow{
		Key:                  entry.Key,
		Name:                 entry.Name,
		Disabled:             entry.Disabled,
		DailyLimit:           entry.DailyLimit,
		TotalQuota:           entry.TotalQuota,
		SpendingLimit:        entry.SpendingLimit,
		ConcurrencyLimit:     entry.ConcurrencyLimit,
		RPMLimit:             entry.RPMLimit,
		TPMLimit:             entry.TPMLimit,
		AllowedModels:        entry.AllowedModels,
		AllowedChannels:      entry.AllowedChannels,
		AllowedChannelGroups: entry.AllowedChannelGroups,
		SystemPrompt:         entry.SystemPrompt,
		CreatedAt:            entry.CreatedAt,
	}
}

const createAPIKeysTableSQL = `
CREATE TABLE IF NOT EXISTS api_keys (
  key               TEXT PRIMARY KEY NOT NULL,
  name              TEXT NOT NULL DEFAULT '',
  disabled          INTEGER NOT NULL DEFAULT 0,
  daily_limit       INTEGER NOT NULL DEFAULT 0,
  total_quota       INTEGER NOT NULL DEFAULT 0,
  spending_limit    REAL NOT NULL DEFAULT 0,
  concurrency_limit INTEGER NOT NULL DEFAULT 0,
  rpm_limit         INTEGER NOT NULL DEFAULT 0,
  tpm_limit         INTEGER NOT NULL DEFAULT 0,
  allowed_models    TEXT NOT NULL DEFAULT '[]',
  allowed_channels  TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  system_prompt     TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL DEFAULT '',
  updated_at        TEXT NOT NULL DEFAULT ''
);
`

func initAPIKeysTable(db *sql.DB) {
	if _, err := db.Exec(createAPIKeysTableSQL); err != nil {
		log.Errorf("usage: create api_keys table: %v", err)
	}
	migrateAPIKeyColumns(db)
	backfillAPIKeyNames(db)
}

func migrateAPIKeyColumns(db *sql.DB) {
	for _, col := range []struct {
		name       string
		definition string
	}{
		{name: "allowed_channels", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{name: "allowed_channel_groups", definition: "TEXT NOT NULL DEFAULT '[]'"},
	} {
		if _, err := db.Exec("ALTER TABLE api_keys ADD COLUMN " + col.name + " " + col.definition); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				log.Warnf("usage: migrate api_keys column %s: %v", col.name, err)
			}
		}
	}
}

func defaultAPIKeyName(index int) string {
	if index < 0 {
		index = 0
	}
	return fmt.Sprintf("api-key-%d", index+1)
}

func backfillAPIKeyNames(db *sql.DB) {
	if db == nil {
		return
	}

	rows, err := db.Query(`SELECT key FROM api_keys WHERE trim(coalesce(name, '')) = '' ORDER BY created_at ASC, key ASC`)
	if err != nil {
		log.Warnf("usage: query unnamed api_keys: %v", err)
		return
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err == nil && strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Warnf("usage: begin api_keys name backfill: %v", err)
		return
	}

	stmt, err := tx.Prepare(`UPDATE api_keys SET name = ?, updated_at = ? WHERE key = ? AND trim(coalesce(name, '')) = ''`)
	if err != nil {
		_ = tx.Rollback()
		log.Warnf("usage: prepare api_keys name backfill: %v", err)
		return
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for idx, key := range keys {
		if _, err := stmt.Exec(defaultAPIKeyName(idx), now, key); err != nil {
			_ = tx.Rollback()
			log.Warnf("usage: update api_keys name backfill for %s: %v", key, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Warnf("usage: commit api_keys name backfill: %v", err)
		return
	}

	log.Infof("usage: backfilled names for %d api_keys", len(keys))
}

// MigrateAPIKeysFromConfig moves API key entries from YAML config into SQLite.
// It only migrates if the api_keys table is empty AND the config has entries.
// After migration, it backs up config.yaml and re-saves it without the API key
// fields so the YAML file stays clean.
func MigrateAPIKeysFromConfig(cfg *config.Config, configFilePath string) int {
	db := getDB()
	if db == nil || cfg == nil {
		return 0
	}

	// Check if SQLite already has data — skip if so (idempotent)
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&count); err != nil {
		log.Errorf("usage: migration count api_keys: %v", err)
		return 0
	}
	if count > 0 {
		// Already migrated. Clear config slices and always try to clean YAML.
		cfg.APIKeys = nil
		cfg.APIKeyEntries = nil
		if configFilePath != "" {
			cleanAPIKeysFromYAML(configFilePath)
		}
		return 0
	}

	// Collect entries to migrate
	seen := make(map[string]struct{})
	var rows []APIKeyRow

	// APIKeyEntries first (richer data)
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
			row.Name = defaultAPIKeyName(len(rows))
		}
		if row.CreatedAt == "" {
			row.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		row.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		rows = append(rows, row)
	}

	// Legacy APIKeys (no metadata)
	for _, k := range cfg.APIKeys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		row := APIKeyRow{
			Key:       trimmed,
			Name:      defaultAPIKeyName(len(rows)),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return 0
	}

	tx, err := db.Begin()
	if err != nil {
		log.Errorf("usage: begin api_keys migration: %v", err)
		return 0
	}

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO api_keys
		(key, name, disabled, daily_limit, total_quota, spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		log.Errorf("usage: prepare api_keys migration: %v", err)
		return 0
	}
	defer stmt.Close()

	imported := 0
	for _, row := range rows {
		modelsJSON, _ := json.Marshal(row.AllowedModels)
		if row.AllowedModels == nil {
			modelsJSON = []byte("[]")
		}
		channelsJSON, _ := json.Marshal(row.AllowedChannels)
		if row.AllowedChannels == nil {
			channelsJSON = []byte("[]")
		}
		channelGroupsJSON, _ := json.Marshal(row.AllowedChannelGroups)
		if row.AllowedChannelGroups == nil {
			channelGroupsJSON = []byte("[]")
		}
		disabledInt := 0
		if row.Disabled {
			disabledInt = 1
		}
		if _, err := stmt.Exec(
			row.Key, row.Name, disabledInt,
			row.DailyLimit, row.TotalQuota, row.SpendingLimit,
			row.ConcurrencyLimit, row.RPMLimit, row.TPMLimit,
			string(modelsJSON), string(channelsJSON), string(channelGroupsJSON), row.SystemPrompt,
			row.CreatedAt, row.UpdatedAt,
		); err != nil {
			_ = tx.Rollback()
			log.Errorf("usage: api_keys migration insert: %v", err)
			return 0
		}
		imported++
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("usage: commit api_keys migration: %v", err)
		return 0
	}

	log.Infof("usage: migrated %d API keys from config to SQLite", imported)

	// Clear config slices so they won't be written back to YAML
	cfg.APIKeys = nil
	cfg.APIKeyEntries = nil

	// Auto-clean the config file to remove stale api-keys/api-key-entries
	if configFilePath != "" {
		if backupConfigForMigration(configFilePath, apiKeysMigrationBackupSuffix) {
			cleanAPIKeysFromYAML(configFilePath)
		}
	}

	return imported
}

// ListAPIKeys retrieves all API key entries from SQLite.
func ListAPIKeys() []APIKeyRow {
	db := getDB()
	if db == nil {
		return nil
	}

	rows, err := db.Query(`SELECT key, name, disabled, daily_limit, total_quota,
		spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_keys ORDER BY created_at ASC`)
	if err != nil {
		log.Errorf("usage: list api_keys: %v", err)
		return nil
	}
	defer rows.Close()

	return scanAPIKeyRows(rows)
}

// GetAPIKey retrieves a single API key entry by key string.
func GetAPIKey(key string) *APIKeyRow {
	db := getDB()
	if db == nil {
		return nil
	}

	row := db.QueryRow(`SELECT key, name, disabled, daily_limit, total_quota,
		spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_keys WHERE key = ?`, key)

	return scanSingleAPIKeyRow(row)
}

// UpsertAPIKey inserts or updates an API key entry.
func UpsertAPIKey(entry APIKeyRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}

	trimmed := strings.TrimSpace(entry.Key)
	if trimmed == "" {
		return fmt.Errorf("key is required")
	}
	entry.Key = trimmed
	entry.Name = strings.TrimSpace(entry.Name)

	modelsJSON, _ := json.Marshal(entry.AllowedModels)
	if entry.AllowedModels == nil {
		modelsJSON = []byte("[]")
	}
	channelsJSON, _ := json.Marshal(entry.AllowedChannels)
	if entry.AllowedChannels == nil {
		channelsJSON = []byte("[]")
	}
	channelGroupsJSON, _ := json.Marshal(entry.AllowedChannelGroups)
	if entry.AllowedChannelGroups == nil {
		channelGroupsJSON = []byte("[]")
	}
	disabledInt := 0
	if entry.Disabled {
		disabledInt = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}

	_, err := db.Exec(`INSERT INTO api_keys
		(key, name, disabled, daily_limit, total_quota, spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			name=excluded.name, disabled=excluded.disabled,
			daily_limit=excluded.daily_limit, total_quota=excluded.total_quota,
			spending_limit=excluded.spending_limit, concurrency_limit=excluded.concurrency_limit,
			rpm_limit=excluded.rpm_limit, tpm_limit=excluded.tpm_limit,
			allowed_models=excluded.allowed_models, allowed_channels=excluded.allowed_channels,
			allowed_channel_groups=excluded.allowed_channel_groups,
			system_prompt=excluded.system_prompt,
			updated_at=excluded.updated_at`,
		trimmed, entry.Name, disabledInt,
		entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit,
		entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
		string(modelsJSON), string(channelsJSON), string(channelGroupsJSON), entry.SystemPrompt,
		entry.CreatedAt, now,
	)
	return err
}

// DeleteAPIKey removes an API key entry by key string.
func DeleteAPIKey(key string) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}
	_, err := db.Exec("DELETE FROM api_keys WHERE key = ?", key)
	return err
}

// ReplaceAllAPIKeys atomically replaces all API keys with the given list.
func ReplaceAllAPIKeys(entries []APIKeyRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM api_keys"); err != nil {
		_ = tx.Rollback()
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_keys
		(key, name, disabled, daily_limit, total_quota, spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" {
			continue
		}
		entry.Key = trimmed
		entry.Name = strings.TrimSpace(entry.Name)
		modelsJSON, _ := json.Marshal(entry.AllowedModels)
		if entry.AllowedModels == nil {
			modelsJSON = []byte("[]")
		}
		channelsJSON, _ := json.Marshal(entry.AllowedChannels)
		if entry.AllowedChannels == nil {
			channelsJSON = []byte("[]")
		}
		channelGroupsJSON, _ := json.Marshal(entry.AllowedChannelGroups)
		if entry.AllowedChannelGroups == nil {
			channelGroupsJSON = []byte("[]")
		}
		disabledInt := 0
		if entry.Disabled {
			disabledInt = 1
		}
		if entry.CreatedAt == "" {
			entry.CreatedAt = now
		}
		if _, err := stmt.Exec(
			trimmed, entry.Name, disabledInt,
			entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit,
			entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
			string(modelsJSON), string(channelsJSON), string(channelGroupsJSON), entry.SystemPrompt,
			entry.CreatedAt, now,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// --- internal helpers ---

func scanAPIKeyRows(rows *sql.Rows) []APIKeyRow {
	var result []APIKeyRow
	for rows.Next() {
		r := scanAPIKeyFromRow(rows)
		if r != nil {
			result = append(result, *r)
		}
	}
	return result
}

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanAPIKeyFromRow(row scannable) *APIKeyRow {
	var r APIKeyRow
	var disabledInt int
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&r.Key, &r.Name, &disabledInt,
		&r.DailyLimit, &r.TotalQuota, &r.SpendingLimit,
		&r.ConcurrencyLimit, &r.RPMLimit, &r.TPMLimit,
		&modelsJSON, &channelsJSON, &channelGroupsJSON, &r.SystemPrompt,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil
	}
	r.Disabled = disabledInt != 0
	if modelsJSON != "" && modelsJSON != "[]" {
		_ = json.Unmarshal([]byte(modelsJSON), &r.AllowedModels)
	}
	if channelsJSON != "" && channelsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelsJSON), &r.AllowedChannels)
	}
	if channelGroupsJSON != "" && channelGroupsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelGroupsJSON), &r.AllowedChannelGroups)
	}
	return &r
}

func scanSingleAPIKeyRow(row *sql.Row) *APIKeyRow {
	var r APIKeyRow
	var disabledInt int
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&r.Key, &r.Name, &disabledInt,
		&r.DailyLimit, &r.TotalQuota, &r.SpendingLimit,
		&r.ConcurrencyLimit, &r.RPMLimit, &r.TPMLimit,
		&modelsJSON, &channelsJSON, &channelGroupsJSON, &r.SystemPrompt,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil
	}
	r.Disabled = disabledInt != 0
	if modelsJSON != "" && modelsJSON != "[]" {
		_ = json.Unmarshal([]byte(modelsJSON), &r.AllowedModels)
	}
	if channelsJSON != "" && channelsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelsJSON), &r.AllowedChannels)
	}
	if channelGroupsJSON != "" && channelGroupsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelGroupsJSON), &r.AllowedChannelGroups)
	}
	return &r
}

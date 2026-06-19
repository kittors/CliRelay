package apikey

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
)

const createAPIKeysTableSQL = `
CREATE TABLE IF NOT EXISTS api_keys (
  key               TEXT PRIMARY KEY NOT NULL,
  id                TEXT NOT NULL DEFAULT '',
  name              TEXT NOT NULL DEFAULT '',
  disabled          INTEGER NOT NULL DEFAULT 0,
  permission_profile_id TEXT NOT NULL DEFAULT '',
  daily_limit       INTEGER NOT NULL DEFAULT 0,
  total_quota       INTEGER NOT NULL DEFAULT 0,
  spending_limit    REAL NOT NULL DEFAULT 0,
  daily_spending_limit REAL NOT NULL DEFAULT 0,
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

CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_id ON api_keys(id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key ON api_keys(key);
`

type APIKeyRow struct {
	ID                   string   `json:"id,omitempty"`
	Key                  string   `json:"key"`
	Name                 string   `json:"name,omitempty"`
	Disabled             bool     `json:"disabled,omitempty"`
	PermissionProfileID  string   `json:"permission-profile-id,omitempty"`
	DailyLimit           int      `json:"daily-limit,omitempty"`
	TotalQuota           int      `json:"total-quota,omitempty"`
	SpendingLimit        float64  `json:"spending-limit,omitempty"`
	DailySpendingLimit   float64  `json:"daily-spending-limit,omitempty"`
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

type PermissionProfileSnapshot struct {
	ID                   string
	DailyLimit           int
	TotalQuota           int
	ConcurrencyLimit     int
	RPMLimit             int
	TPMLimit             int
	AllowedModels        []string
	AllowedChannels      []string
	AllowedChannelGroups []string
	SystemPrompt         string
}

type Store struct {
	db *sql.DB
}

type scanner interface {
	Scan(dest ...any) error
}

func NewStore(db *sql.DB) Store {
	return Store{db: db}
}

func InitTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createAPIKeysTableSQL); err != nil {
		log.Errorf("sqlite/apikey: create api_keys table: %v", err)
	}
	migrateColumns(db)
	backfillIDs(db)
	backfillNames(db)
}

func BackfillNames(db *sql.DB) {
	if db == nil {
		return
	}
	backfillNames(db)
}

func (r APIKeyRow) ToConfigEntry() config.APIKeyEntry {
	return config.APIKeyEntry{
		ID:                   r.ID,
		Key:                  r.Key,
		Name:                 r.Name,
		Disabled:             r.Disabled,
		PermissionProfileID:  r.PermissionProfileID,
		DailyLimit:           r.DailyLimit,
		TotalQuota:           r.TotalQuota,
		SpendingLimit:        r.SpendingLimit,
		DailySpendingLimit:   r.DailySpendingLimit,
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

func APIKeyRowFromConfig(entry config.APIKeyEntry) APIKeyRow {
	return APIKeyRow{
		ID:                   entry.ID,
		Key:                  entry.Key,
		Name:                 entry.Name,
		Disabled:             entry.Disabled,
		PermissionProfileID:  entry.PermissionProfileID,
		DailyLimit:           entry.DailyLimit,
		TotalQuota:           entry.TotalQuota,
		SpendingLimit:        entry.SpendingLimit,
		DailySpendingLimit:   entry.DailySpendingLimit,
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

func DefaultAPIKeyName(index int) string {
	if index < 0 {
		index = 0
	}
	return fmt.Sprintf("api-key-%d", index+1)
}

func (s Store) Available() bool {
	return s.db != nil
}

func (s Store) Count() int64 {
	if s.db == nil {
		return 0
	}

	var count int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM api_keys").Scan(&count); err != nil {
		log.Warnf("sqlite/apikey: count api_keys: %v", err)
		return 0
	}
	return count
}

func (s Store) List() []APIKeyRow {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`SELECT key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_keys ORDER BY created_at ASC`)
	if err != nil {
		log.Errorf("sqlite/apikey: list api_keys: %v", err)
		return nil
	}
	defer rows.Close()

	return scanAPIKeyRows(rows)
}

func (s Store) Get(key string) *APIKeyRow {
	if s.db == nil {
		return nil
	}

	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil
	}

	row := s.db.QueryRow(`SELECT key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_keys WHERE key = ?`, trimmed)
	entry, ok := scanAPIKeyRow(row)
	if !ok {
		return nil
	}
	return entry
}

func (s Store) GetByID(id string) *APIKeyRow {
	if s.db == nil {
		return nil
	}

	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil
	}

	row := s.db.QueryRow(`SELECT key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_keys WHERE id = ?`, trimmed)
	entry, ok := scanAPIKeyRow(row)
	if !ok {
		return nil
	}
	return entry
}

func (s Store) Upsert(entry APIKeyRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}

	entry = normalizeRow(entry)
	if entry.Key == "" {
		return fmt.Errorf("key is required")
	}
	if entry.ID == "" {
		if existing := s.Get(entry.Key); existing != nil && existing.ID != "" {
			entry.ID = existing.ID
		} else {
			entry.ID = uuid.NewString()
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}

	disabledInt := 0
	if entry.Disabled {
		disabledInt = 1
	}

	_, err := s.db.Exec(`INSERT INTO api_keys
		(key, id, name, disabled, permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			id=excluded.id,
			name=excluded.name, disabled=excluded.disabled,
			permission_profile_id=excluded.permission_profile_id,
			daily_limit=excluded.daily_limit, total_quota=excluded.total_quota,
			spending_limit=excluded.spending_limit, daily_spending_limit=excluded.daily_spending_limit, concurrency_limit=excluded.concurrency_limit,
			rpm_limit=excluded.rpm_limit, tpm_limit=excluded.tpm_limit,
			allowed_models=excluded.allowed_models, allowed_channels=excluded.allowed_channels,
			allowed_channel_groups=excluded.allowed_channel_groups,
			system_prompt=excluded.system_prompt,
			updated_at=excluded.updated_at`,
		entry.Key, entry.ID, entry.Name, disabledInt, entry.PermissionProfileID,
		entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit, entry.DailySpendingLimit,
		entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
		mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
		mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
		entry.CreatedAt, now,
	)
	return err
}

func (s Store) UpdateByID(entry APIKeyRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}

	entry = normalizeRow(entry)
	if entry.ID == "" {
		return fmt.Errorf("id is required")
	}
	if entry.Key == "" {
		return fmt.Errorf("key is required")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		if existing := s.GetByID(entry.ID); existing != nil && existing.CreatedAt != "" {
			entry.CreatedAt = existing.CreatedAt
		} else {
			entry.CreatedAt = now
		}
	}

	disabledInt := 0
	if entry.Disabled {
		disabledInt = 1
	}

	_, err := s.db.Exec(`UPDATE api_keys SET
		key = ?, name = ?, disabled = ?, permission_profile_id = ?, daily_limit = ?, total_quota = ?,
		spending_limit = ?, daily_spending_limit = ?, concurrency_limit = ?, rpm_limit = ?, tpm_limit = ?,
		allowed_models = ?, allowed_channels = ?, allowed_channel_groups = ?, system_prompt = ?,
		created_at = ?, updated_at = ?
		WHERE id = ?`,
		entry.Key, entry.Name, disabledInt, entry.PermissionProfileID, entry.DailyLimit, entry.TotalQuota,
		entry.SpendingLimit, entry.DailySpendingLimit, entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
		mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
		mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
		entry.CreatedAt, now, entry.ID,
	)
	return err
}

func (s Store) Delete(key string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return fmt.Errorf("key is required")
	}
	_, err := s.db.Exec("DELETE FROM api_keys WHERE key = ?", trimmed)
	return err
}

func (s Store) DeleteByID(id string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return fmt.Errorf("id is required")
	}
	_, err := s.db.Exec("DELETE FROM api_keys WHERE id = ?", trimmed)
	return err
}

func (s Store) ReplaceAll(entries []APIKeyRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	existingIDsByKey := make(map[string]string)
	for _, row := range s.List() {
		key := strings.TrimSpace(row.Key)
		id := strings.TrimSpace(row.ID)
		if key == "" || id == "" {
			continue
		}
		existingIDsByKey[key] = id
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM api_keys"); err != nil {
		_ = tx.Rollback()
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_keys
		(key, id, name, disabled, permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, entry := range entries {
		entry = normalizeRow(entry)
		if entry.Key == "" {
			continue
		}
		if entry.ID == "" {
			if existingID := existingIDsByKey[entry.Key]; existingID != "" {
				entry.ID = existingID
			} else {
				entry.ID = uuid.NewString()
			}
		}
		if entry.CreatedAt == "" {
			entry.CreatedAt = now
		}
		disabledInt := 0
		if entry.Disabled {
			disabledInt = 1
		}
		if _, err := stmt.Exec(
			entry.Key, entry.ID, entry.Name, disabledInt, entry.PermissionProfileID,
			entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit, entry.DailySpendingLimit,
			entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
			mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
			mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
			entry.CreatedAt, now,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func EffectiveAPIKeyRowWithProfiles(row APIKeyRow, profiles []PermissionProfileSnapshot) APIKeyRow {
	profileID := strings.TrimSpace(row.PermissionProfileID)
	if profileID == "" {
		return row
	}

	var matched *PermissionProfileSnapshot
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) == profileID {
			copy := profile
			matched = &copy
			break
		}
	}
	if matched == nil {
		return row
	}

	row.PermissionProfileID = profileID
	row.DailyLimit = matched.DailyLimit
	row.TotalQuota = matched.TotalQuota
	row.SpendingLimit = 0
	row.DailySpendingLimit = 0
	row.ConcurrencyLimit = matched.ConcurrencyLimit
	row.RPMLimit = matched.RPMLimit
	row.TPMLimit = matched.TPMLimit
	row.AllowedModels = append([]string(nil), matched.AllowedModels...)
	row.AllowedChannels = append([]string(nil), matched.AllowedChannels...)
	row.AllowedChannelGroups = append([]string(nil), matched.AllowedChannelGroups...)
	row.SystemPrompt = matched.SystemPrompt
	return row
}

func EffectiveAPIKeyRowsWithProfiles(rows []APIKeyRow, profiles []PermissionProfileSnapshot) []APIKeyRow {
	if len(rows) == 0 {
		return rows
	}
	out := make([]APIKeyRow, len(rows))
	for idx, row := range rows {
		out[idx] = EffectiveAPIKeyRowWithProfiles(row, profiles)
	}
	return out
}

func migrateColumns(db *sql.DB) {
	for _, col := range []struct {
		name       string
		definition string
	}{
		{name: "id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "permission_profile_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "allowed_channels", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{name: "allowed_channel_groups", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{name: "daily_spending_limit", definition: "REAL NOT NULL DEFAULT 0"},
	} {
		if _, err := db.Exec("ALTER TABLE api_keys ADD COLUMN " + col.name + " " + col.definition); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
				log.Warnf("sqlite/apikey: migrate api_keys column %s: %v", col.name, err)
			}
		}
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_id ON api_keys(id)"); err != nil {
		log.Warnf("sqlite/apikey: ensure api_keys id index: %v", err)
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_key ON api_keys(key)"); err != nil {
		log.Warnf("sqlite/apikey: ensure api_keys key index: %v", err)
	}
}

func backfillIDs(db *sql.DB) {
	rows, err := db.Query(`SELECT key FROM api_keys WHERE trim(coalesce(id, '')) = '' ORDER BY created_at ASC, key ASC`)
	if err != nil {
		log.Warnf("sqlite/apikey: query api_keys without id: %v", err)
		return
	}
	defer rows.Close()

	keys := make([]string, 0)
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
		log.Warnf("sqlite/apikey: begin api_keys id backfill: %v", err)
		return
	}

	stmt, err := tx.Prepare(`UPDATE api_keys SET id = ?, updated_at = ? WHERE key = ? AND trim(coalesce(id, '')) = ''`)
	if err != nil {
		_ = tx.Rollback()
		log.Warnf("sqlite/apikey: prepare api_keys id backfill: %v", err)
		return
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, key := range keys {
		if _, err := stmt.Exec(uuid.NewString(), now, key); err != nil {
			_ = tx.Rollback()
			log.Warnf("sqlite/apikey: update api_keys id backfill for %s: %v", key, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Warnf("sqlite/apikey: commit api_keys id backfill: %v", err)
		return
	}

	log.Infof("sqlite/apikey: backfilled ids for %d api_keys", len(keys))
}

func backfillNames(db *sql.DB) {
	rows, err := db.Query(`SELECT key FROM api_keys WHERE trim(coalesce(name, '')) = '' ORDER BY created_at ASC, key ASC`)
	if err != nil {
		log.Warnf("sqlite/apikey: query unnamed api_keys: %v", err)
		return
	}
	defer rows.Close()

	keys := make([]string, 0)
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
		log.Warnf("sqlite/apikey: begin api_keys name backfill: %v", err)
		return
	}

	stmt, err := tx.Prepare(`UPDATE api_keys SET name = ?, updated_at = ? WHERE key = ? AND trim(coalesce(name, '')) = ''`)
	if err != nil {
		_ = tx.Rollback()
		log.Warnf("sqlite/apikey: prepare api_keys name backfill: %v", err)
		return
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for idx, key := range keys {
		if _, err := stmt.Exec(DefaultAPIKeyName(idx), now, key); err != nil {
			_ = tx.Rollback()
			log.Warnf("sqlite/apikey: update api_keys name backfill for %s: %v", key, err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Warnf("sqlite/apikey: commit api_keys name backfill: %v", err)
		return
	}

	log.Infof("sqlite/apikey: backfilled names for %d api_keys", len(keys))
}

func normalizeRow(row APIKeyRow) APIKeyRow {
	row.ID = strings.TrimSpace(row.ID)
	row.Key = strings.TrimSpace(row.Key)
	row.Name = strings.TrimSpace(row.Name)
	row.PermissionProfileID = strings.TrimSpace(row.PermissionProfileID)
	return row
}

func mustJSONStringList(values []string) string {
	if values == nil {
		return "[]"
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func scanAPIKeyRows(rows *sql.Rows) []APIKeyRow {
	result := make([]APIKeyRow, 0)
	for rows.Next() {
		entry, ok := scanAPIKeyRow(rows)
		if ok {
			result = append(result, *entry)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("sqlite/apikey: scan api_keys rows: %v", err)
	}
	return result
}

func scanAPIKeyRow(row scanner) (*APIKeyRow, bool) {
	var entry APIKeyRow
	var disabledInt int
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&entry.Key, &entry.Name, &disabledInt,
		&entry.ID,
		&entry.DailyLimit, &entry.TotalQuota, &entry.PermissionProfileID, &entry.SpendingLimit, &entry.DailySpendingLimit,
		&entry.ConcurrencyLimit, &entry.RPMLimit, &entry.TPMLimit,
		&modelsJSON, &channelsJSON, &channelGroupsJSON, &entry.SystemPrompt,
		&entry.CreatedAt, &entry.UpdatedAt,
	); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/apikey: scan api_keys row: %v", err)
		}
		return nil, false
	}
	entry.Disabled = disabledInt != 0
	entry.PermissionProfileID = strings.TrimSpace(entry.PermissionProfileID)
	if modelsJSON != "" && modelsJSON != "[]" {
		_ = json.Unmarshal([]byte(modelsJSON), &entry.AllowedModels)
	}
	if channelsJSON != "" && channelsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelsJSON), &entry.AllowedChannels)
	}
	if channelGroupsJSON != "" && channelGroupsJSON != "[]" {
		_ = json.Unmarshal([]byte(channelGroupsJSON), &entry.AllowedChannelGroups)
	}
	return &entry, true
}

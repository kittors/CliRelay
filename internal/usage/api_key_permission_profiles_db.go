package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const apiKeyPermissionProfilesMigrationBackupSuffix = ".pre-api-key-permission-profiles-sqlite-migration"

type APIKeyPermissionProfileRow struct {
	ID                   string   `json:"id" yaml:"id"`
	Name                 string   `json:"name" yaml:"name"`
	DailyLimit           int      `json:"daily-limit" yaml:"daily-limit"`
	TotalQuota           int      `json:"total-quota" yaml:"total-quota"`
	ConcurrencyLimit     int      `json:"concurrency-limit" yaml:"concurrency-limit"`
	RPMLimit             int      `json:"rpm-limit" yaml:"rpm-limit"`
	TPMLimit             int      `json:"tpm-limit" yaml:"tpm-limit"`
	AllowedModels        []string `json:"allowed-models" yaml:"allowed-models"`
	AllowedChannels      []string `json:"allowed-channels" yaml:"allowed-channels"`
	AllowedChannelGroups []string `json:"allowed-channel-groups" yaml:"allowed-channel-groups"`
	SystemPrompt         string   `json:"system-prompt" yaml:"system-prompt"`
	CreatedAt            string   `json:"created-at,omitempty" yaml:"created-at,omitempty"`
	UpdatedAt            string   `json:"updated-at,omitempty" yaml:"updated-at,omitempty"`
}

const createAPIKeyPermissionProfilesTableSQL = `
CREATE TABLE IF NOT EXISTS api_key_permission_profiles (
  id                     TEXT PRIMARY KEY NOT NULL,
  name                   TEXT NOT NULL DEFAULT '',
  daily_limit            INTEGER NOT NULL DEFAULT 0,
  total_quota            INTEGER NOT NULL DEFAULT 0,
  concurrency_limit      INTEGER NOT NULL DEFAULT 0,
  rpm_limit              INTEGER NOT NULL DEFAULT 0,
  tpm_limit              INTEGER NOT NULL DEFAULT 0,
  allowed_models         TEXT NOT NULL DEFAULT '[]',
  allowed_channels       TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  system_prompt          TEXT NOT NULL DEFAULT '',
  created_at             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL DEFAULT ''
);
`

func initAPIKeyPermissionProfilesTable(db *sql.DB) {
	if _, err := db.Exec(createAPIKeyPermissionProfilesTableSQL); err != nil {
		log.Errorf("usage: create api_key_permission_profiles table: %v", err)
	}
}

func ListAPIKeyPermissionProfiles() []APIKeyPermissionProfileRow {
	db := getDB()
	if db == nil {
		return nil
	}

	rows, err := db.Query(`SELECT id, name, daily_limit, total_quota, concurrency_limit,
		rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups,
		system_prompt, created_at, updated_at
		FROM api_key_permission_profiles ORDER BY created_at ASC, id ASC`)
	if err != nil {
		log.Errorf("usage: list api_key_permission_profiles: %v", err)
		return nil
	}
	defer rows.Close()

	var result []APIKeyPermissionProfileRow
	for rows.Next() {
		r := scanAPIKeyPermissionProfileFromRow(rows)
		if r != nil {
			result = append(result, *r)
		}
	}
	return result
}

func ReplaceAllAPIKeyPermissionProfiles(profiles []APIKeyPermissionProfileRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("database not initialised")
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM api_key_permission_profiles"); err != nil {
		_ = tx.Rollback()
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_key_permission_profiles
		(id, name, daily_limit, total_quota, concurrency_limit, rpm_limit, tpm_limit,
		 allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		profile = normalizeAPIKeyPermissionProfile(profile)
		if profile.ID == "" {
			_ = tx.Rollback()
			return fmt.Errorf("id is required")
		}
		if profile.Name == "" {
			_ = tx.Rollback()
			return fmt.Errorf("name is required")
		}
		if _, exists := seen[profile.ID]; exists {
			_ = tx.Rollback()
			return fmt.Errorf("duplicate id %q", profile.ID)
		}
		seen[profile.ID] = struct{}{}
		if profile.CreatedAt == "" {
			profile.CreatedAt = now
		}
		profile.UpdatedAt = now

		modelsJSON := mustJSONStringList(profile.AllowedModels)
		channelsJSON := mustJSONStringList(profile.AllowedChannels)
		channelGroupsJSON := mustJSONStringList(profile.AllowedChannelGroups)
		if _, err := stmt.Exec(
			profile.ID, profile.Name, profile.DailyLimit, profile.TotalQuota,
			profile.ConcurrencyLimit, profile.RPMLimit, profile.TPMLimit,
			modelsJSON, channelsJSON, channelGroupsJSON, profile.SystemPrompt,
			profile.CreatedAt, profile.UpdatedAt,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func MigrateAPIKeyPermissionProfilesFromYAML(configFilePath string) int {
	db := getDB()
	if db == nil || strings.TrimSpace(configFilePath) == "" {
		return 0
	}

	data, err := os.ReadFile(configFilePath)
	if err != nil {
		log.Warnf("usage: read config for api_key_permission_profiles migration: %v", err)
		return 0
	}

	var root struct {
		Profiles []APIKeyPermissionProfileRow `yaml:"api-key-permission-profiles"`
	}
	if err := yaml.Unmarshal(data, &root); err != nil {
		log.Warnf("usage: parse config for api_key_permission_profiles migration: %v", err)
		return 0
	}

	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM api_key_permission_profiles").Scan(&count); err != nil {
		log.Errorf("usage: migration count api_key_permission_profiles: %v", err)
		return 0
	}
	if count > 0 {
		cleanAPIKeyPermissionProfilesFromYAML(configFilePath)
		return 0
	}

	profiles := make([]APIKeyPermissionProfileRow, 0, len(root.Profiles))
	for _, profile := range root.Profiles {
		profile = normalizeAPIKeyPermissionProfile(profile)
		if profile.ID == "" || profile.Name == "" {
			continue
		}
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		cleanAPIKeyPermissionProfilesFromYAML(configFilePath)
		return 0
	}

	if err := ReplaceAllAPIKeyPermissionProfiles(profiles); err != nil {
		log.Errorf("usage: migrate api_key_permission_profiles: %v", err)
		return 0
	}

	if backupConfigForMigration(configFilePath, apiKeyPermissionProfilesMigrationBackupSuffix) {
		cleanAPIKeyPermissionProfilesFromYAML(configFilePath)
	}
	log.Infof("usage: migrated %d API key permission profile(s) from config to SQLite", len(profiles))
	return len(profiles)
}

func cleanAPIKeyPermissionProfilesFromYAML(configFilePath string) {
	cleanConfigKeysFromYAML(configFilePath, map[string]bool{
		"api-key-permission-profiles": true,
	}, "api_key_permission_profiles")
}

func normalizeAPIKeyPermissionProfile(profile APIKeyPermissionProfileRow) APIKeyPermissionProfileRow {
	profile.ID = strings.TrimSpace(profile.ID)
	profile.Name = strings.TrimSpace(profile.Name)
	profile.DailyLimit = normalizeNonNegativeInt(profile.DailyLimit)
	profile.TotalQuota = normalizeNonNegativeInt(profile.TotalQuota)
	profile.ConcurrencyLimit = normalizeNonNegativeInt(profile.ConcurrencyLimit)
	profile.RPMLimit = normalizeNonNegativeInt(profile.RPMLimit)
	profile.TPMLimit = normalizeNonNegativeInt(profile.TPMLimit)
	profile.AllowedModels = normalizeStringSlice(profile.AllowedModels)
	profile.AllowedChannels = normalizeStringSlice(profile.AllowedChannels)
	profile.AllowedChannelGroups = normalizeStringSlice(profile.AllowedChannelGroups)
	profile.SystemPrompt = strings.TrimSpace(profile.SystemPrompt)
	return profile
}

func normalizeNonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func normalizeStringSlice(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	if result == nil {
		return []string{}
	}
	return result
}

func mustJSONStringList(values []string) string {
	normalized := normalizeStringSlice(values)
	data, err := json.Marshal(normalized)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func scanAPIKeyPermissionProfileFromRow(row scannable) *APIKeyPermissionProfileRow {
	var r APIKeyPermissionProfileRow
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&r.ID, &r.Name, &r.DailyLimit, &r.TotalQuota, &r.ConcurrencyLimit,
		&r.RPMLimit, &r.TPMLimit, &modelsJSON, &channelsJSON, &channelGroupsJSON,
		&r.SystemPrompt, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil
	}
	r.AllowedModels = decodeJSONStringList(modelsJSON)
	r.AllowedChannels = decodeJSONStringList(channelsJSON)
	r.AllowedChannelGroups = decodeJSONStringList(channelGroupsJSON)
	return &r
}

func decodeJSONStringList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return normalizeStringSlice(values)
}

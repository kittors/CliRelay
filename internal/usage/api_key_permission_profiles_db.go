package usage

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"strings"

	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const apiKeyPermissionProfilesMigrationBackupSuffix = ".pre-api-key-permission-profiles-sqlite-migration"

type APIKeyPermissionProfileRow = sqlapikey.PermissionProfileRow
type APIKeyPermissionProfileSyncResult = sqlapikey.PermissionProfileSyncResult

func initAPIKeyPermissionProfilesTable(db *sql.DB) {
	sqlapikey.InitPermissionProfilesTable(db)
}

func apiKeyPermissionProfileStore() sqlapikey.Store {
	return sqlapikey.NewStore(getDB())
}

func apiKeyPermissionProfileStoreForTenant(tenantID string) sqlapikey.Store {
	return sqlapikey.NewTenantStore(getDB(), tenantID)
}

func ListAPIKeyPermissionProfiles() []APIKeyPermissionProfileRow {
	return apiKeyPermissionProfileStore().ListPermissionProfiles()
}

func ReplaceAllAPIKeyPermissionProfiles(profiles []APIKeyPermissionProfileRow) error {
	return apiKeyPermissionProfileStore().ReplaceAllPermissionProfiles(profiles)
}

func ListAPIKeyPermissionProfilesForTenant(tenantID string) []APIKeyPermissionProfileRow {
	return apiKeyPermissionProfileStoreForTenant(tenantID).ListPermissionProfiles()
}

func ReplaceAllAPIKeyPermissionProfilesForTenant(tenantID string, profiles []APIKeyPermissionProfileRow) error {
	return apiKeyPermissionProfileStoreForTenant(tenantID).ReplaceAllPermissionProfiles(profiles)
}

func ReplaceAllAPIKeyPermissionProfilesForTenantAndSyncEndUsers(tenantID string, profiles []APIKeyPermissionProfileRow) (int64, error) {
	return apiKeyPermissionProfileStoreForTenant(tenantID).ReplaceAllPermissionProfilesAndSyncEndUsers(profiles)
}

func ReplaceAllAPIKeyPermissionProfilesForTenantWithCaps(tenantID string, profiles []APIKeyPermissionProfileRow, syncEndUsers bool) (sqlapikey.PermissionProfileSyncResult, error) {
	return apiKeyPermissionProfileStoreForTenant(tenantID).ReplaceAllPermissionProfilesWithCaps(profiles, syncEndUsers)
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
	log.Infof("usage: migrated %d API key permission profile(s) from config to the database", len(profiles))
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
	// Spending limits are whole USD dollars; ceil partial input.
	if profile.DailySpendingLimit <= 0 || math.IsNaN(profile.DailySpendingLimit) || math.IsInf(profile.DailySpendingLimit, 0) {
		profile.DailySpendingLimit = 0
	} else {
		profile.DailySpendingLimit = math.Ceil(profile.DailySpendingLimit)
	}
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

func mustJSONStringList(values []string) string {
	normalized := normalizeStringSlice(values)
	data, err := json.Marshal(normalized)
	if err != nil {
		return "[]"
	}
	return string(data)
}

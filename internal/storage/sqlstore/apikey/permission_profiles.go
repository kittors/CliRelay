package apikey

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	log "github.com/sirupsen/logrus"
)

const createAPIKeyPermissionProfilesTableSQL = `
CREATE TABLE IF NOT EXISTS api_key_permission_profiles (
  tenant_id              TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id                     TEXT NOT NULL,
  name                   TEXT NOT NULL DEFAULT '',
  daily_limit            INTEGER NOT NULL DEFAULT 0,
  total_quota            INTEGER NOT NULL DEFAULT 0,
  daily_spending_limit   REAL NOT NULL DEFAULT 0,
  five_hour_spending_limit REAL NOT NULL DEFAULT 0,
  weekly_spending_limit  REAL NOT NULL DEFAULT 0,
  monthly_spending_limit REAL NOT NULL DEFAULT 0,
  concurrency_limit      INTEGER NOT NULL DEFAULT 0,
  rpm_limit              INTEGER NOT NULL DEFAULT 0,
  tpm_limit              INTEGER NOT NULL DEFAULT 0,
  allowed_models         TEXT NOT NULL DEFAULT '[]',
  allowed_channels       TEXT NOT NULL DEFAULT '[]',
  allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
  system_prompt          TEXT NOT NULL DEFAULT '',
  created_at             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id)
);
`

type PermissionProfileRow struct {
	TenantID             string                     `json:"tenant_id,omitempty" yaml:"tenant_id,omitempty"`
	ID                   string                     `json:"id" yaml:"id"`
	Name                 string                     `json:"name" yaml:"name"`
	DailyLimit           int                        `json:"daily-limit" yaml:"daily-limit"`
	TotalQuota           int                        `json:"total-quota" yaml:"total-quota"`
	DailySpendingLimit   float64                    `json:"daily-spending-limit" yaml:"daily-spending-limit"`
	PeriodSpendingLimits quota.PeriodSpendingLimits `json:"period-spending-limits" yaml:"period-spending-limits"`
	ConcurrencyLimit     int                        `json:"concurrency-limit" yaml:"concurrency-limit"`
	RPMLimit             int                        `json:"rpm-limit" yaml:"rpm-limit"`
	TPMLimit             int                        `json:"tpm-limit" yaml:"tpm-limit"`
	AllowedModels        []string                   `json:"allowed-models" yaml:"allowed-models"`
	AllowedChannels      []string                   `json:"allowed-channels" yaml:"allowed-channels"`
	AllowedChannelGroups []string                   `json:"allowed-channel-groups" yaml:"allowed-channel-groups"`
	SystemPrompt         string                     `json:"system-prompt" yaml:"system-prompt"`
	CreatedAt            string                     `json:"created-at,omitempty" yaml:"created-at,omitempty"`
	UpdatedAt            string                     `json:"updated-at,omitempty" yaml:"updated-at,omitempty"`
}

type PermissionProfileSyncResult struct {
	AppliedCount int64             `json:"applied_count"`
	CappedKeys   []quota.CappedKey `json:"capped-keys,omitempty"`
}

func InitPermissionProfilesTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createAPIKeyPermissionProfilesTableSQL); err != nil {
		log.Errorf("sqlite/apikey: create api_key_permission_profiles table: %v", err)
	}
	if _, err := db.Exec("ALTER TABLE api_key_permission_profiles ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
		log.Warnf("sqlite/apikey: migrate api_key_permission_profiles tenant_id: %v", err)
	}
	for _, column := range []string{"daily_spending_limit", "five_hour_spending_limit", "weekly_spending_limit", "monthly_spending_limit"} {
		if _, err := db.Exec("ALTER TABLE api_key_permission_profiles ADD COLUMN " + column + " REAL NOT NULL DEFAULT 0"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			log.Warnf("sqlite/apikey: migrate api_key_permission_profiles %s: %v", column, err)
		}
	}
	if err := migratePermissionProfilesTenantPrimaryKey(db); err != nil {
		log.Errorf("sqlite/apikey: migrate api_key_permission_profiles tenant primary key: %v", err)
	}
	for _, table := range []string{"end_users", "api_keys"} {
		for _, column := range []string{"five_hour_spending_limit", "weekly_spending_limit", "monthly_spending_limit"} {
			if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " REAL NOT NULL DEFAULT 0"); err != nil {
				message := strings.ToLower(err.Error())
				if !strings.Contains(message, "duplicate") && !strings.Contains(message, "no such table") {
					log.Warnf("sqlite/apikey: migrate %s.%s: %v", table, column, err)
				}
			}
		}
	}
}

func migratePermissionProfilesTenantPrimaryKey(db *sql.DB) error {
	driverType := strings.ToLower(fmt.Sprintf("%T", db.Driver()))
	if strings.Contains(driverType, "postgres") || strings.Contains(driverType, "compatdriver") {
		return nil
	}
	rows, err := db.Query("PRAGMA table_info(api_key_permission_profiles)")
	if err != nil {
		// PostgreSQL uses its own migration for the composite primary key.
		return nil
	}
	defer rows.Close()
	primaryKeyColumns := map[int]string{}
	for rows.Next() {
		var cid, notNull, primaryKeyOrder int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKeyOrder); err != nil {
			return err
		}
		if primaryKeyOrder > 0 {
			primaryKeyColumns[primaryKeyOrder] = strings.ToLower(strings.TrimSpace(name))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if primaryKeyColumns[1] == "tenant_id" && primaryKeyColumns[2] == "id" && len(primaryKeyColumns) == 2 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`DROP TABLE IF EXISTS api_key_permission_profiles_tenant_pk`); err != nil {
		return err
	}
	if _, err = tx.Exec(`
		CREATE TABLE api_key_permission_profiles_tenant_pk (
			tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
			id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', daily_limit INTEGER NOT NULL DEFAULT 0,
			total_quota INTEGER NOT NULL DEFAULT 0, daily_spending_limit REAL NOT NULL DEFAULT 0,
			five_hour_spending_limit REAL NOT NULL DEFAULT 0, weekly_spending_limit REAL NOT NULL DEFAULT 0, monthly_spending_limit REAL NOT NULL DEFAULT 0,
			concurrency_limit INTEGER NOT NULL DEFAULT 0, rpm_limit INTEGER NOT NULL DEFAULT 0,
			tpm_limit INTEGER NOT NULL DEFAULT 0, allowed_models TEXT NOT NULL DEFAULT '[]',
			allowed_channels TEXT NOT NULL DEFAULT '[]', allowed_channel_groups TEXT NOT NULL DEFAULT '[]',
			system_prompt TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id)
		)
	`); err != nil {
		return err
	}
	if _, err = tx.Exec(`
		INSERT INTO api_key_permission_profiles_tenant_pk (
			tenant_id, id, name, daily_limit, total_quota, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit,
			rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		)
		SELECT CASE WHEN trim(coalesce(tenant_id, '')) = '' THEN ? ELSE tenant_id END,
			id, name, daily_limit, total_quota, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit,
			rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at
		FROM api_key_permission_profiles
	`, systemTenantID); err != nil {
		return err
	}
	if _, err = tx.Exec(`DROP TABLE api_key_permission_profiles`); err != nil {
		return err
	}
	if _, err = tx.Exec(`ALTER TABLE api_key_permission_profiles_tenant_pk RENAME TO api_key_permission_profiles`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) ListPermissionProfiles() []PermissionProfileRow {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`SELECT id, name, daily_limit, total_quota, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit,
		rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups,
		system_prompt, created_at, updated_at
		FROM api_key_permission_profiles WHERE tenant_id = ? ORDER BY created_at ASC, id ASC`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/apikey: list api_key_permission_profiles: %v", err)
		return nil
	}
	defer rows.Close()

	result := make([]PermissionProfileRow, 0)
	for rows.Next() {
		profile, ok := scanPermissionProfileRow(rows)
		if ok {
			profile.TenantID = s.tenantID
			result = append(result, *profile)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("sqlite/apikey: scan api_key_permission_profiles rows: %v", err)
	}
	return result
}

func (s Store) ReplaceAllPermissionProfiles(profiles []PermissionProfileRow) error {
	_, err := s.replaceAllPermissionProfiles(profiles, false)
	return err
}

func (s Store) ReplaceAllPermissionProfilesAndSyncEndUsers(profiles []PermissionProfileRow) (int64, error) {
	result, err := s.replaceAllPermissionProfiles(profiles, true)
	return result.AppliedCount, err
}

func (s Store) ReplaceAllPermissionProfilesWithCaps(profiles []PermissionProfileRow, syncEndUsers bool) (PermissionProfileSyncResult, error) {
	return s.replaceAllPermissionProfiles(profiles, syncEndUsers)
}

func (s Store) replaceAllPermissionProfiles(profiles []PermissionProfileRow, syncEndUsers bool) (PermissionProfileSyncResult, error) {
	if s.db == nil {
		return PermissionProfileSyncResult{}, fmt.Errorf("database not initialised")
	}
	for _, table := range []string{"end_users", "api_keys"} {
		for _, column := range []string{"five_hour_spending_limit", "weekly_spending_limit", "monthly_spending_limit"} {
			if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " REAL NOT NULL DEFAULT 0"); err != nil {
				message := strings.ToLower(err.Error())
				if !strings.Contains(message, "duplicate") && !strings.Contains(message, "no such table") {
					return PermissionProfileSyncResult{}, err
				}
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return PermissionProfileSyncResult{}, err
	}
	if err := lockBoundEndUsersForProfileUpdate(tx, s.tenantID); err != nil {
		_ = tx.Rollback()
		return PermissionProfileSyncResult{}, err
	}

	if _, err := tx.Exec("DELETE FROM api_key_permission_profiles WHERE tenant_id = ?", s.tenantID); err != nil {
		_ = tx.Rollback()
		return PermissionProfileSyncResult{}, err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_key_permission_profiles
		(tenant_id, id, name, daily_limit, total_quota, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		 allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return PermissionProfileSyncResult{}, err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		profile = normalizePermissionProfile(profile)
		if profile.ID == "" {
			_ = tx.Rollback()
			return PermissionProfileSyncResult{}, fmt.Errorf("id is required")
		}
		if profile.Name == "" {
			_ = tx.Rollback()
			return PermissionProfileSyncResult{}, fmt.Errorf("name is required")
		}
		if _, exists := seen[profile.ID]; exists {
			_ = tx.Rollback()
			return PermissionProfileSyncResult{}, fmt.Errorf("duplicate id %q", profile.ID)
		}
		seen[profile.ID] = struct{}{}
		if profile.CreatedAt == "" {
			profile.CreatedAt = now
		}
		profile.UpdatedAt = now

		if _, err := stmt.Exec(
			s.tenantID, profile.ID, profile.Name, profile.DailyLimit, profile.TotalQuota, profile.DailySpendingLimit,
			profile.PeriodSpendingLimits.FiveHour, profile.PeriodSpendingLimits.Week, profile.PeriodSpendingLimits.Month, profile.ConcurrencyLimit, profile.RPMLimit, profile.TPMLimit,
			mustJSONStringList(profile.AllowedModels), mustJSONStringList(profile.AllowedChannels),
			mustJSONStringList(profile.AllowedChannelGroups), profile.SystemPrompt,
			profile.CreatedAt, profile.UpdatedAt,
		); err != nil {
			_ = tx.Rollback()
			return PermissionProfileSyncResult{}, err
		}
	}

	appliedCount := int64(0)
	if syncEndUsers {
		for _, profile := range profiles {
			profile = normalizePermissionProfile(profile)
			result, syncErr := tx.Exec(`
				UPDATE end_users SET
					permission_profile_id = ?, daily_limit = ?, total_quota = ?,
					daily_spending_limit = ?, five_hour_spending_limit = ?, weekly_spending_limit = ?, monthly_spending_limit = ?, concurrency_limit = ?, rpm_limit = ?, tpm_limit = ?,
					allowed_models = ?, allowed_channels = ?, allowed_channel_groups = ?, system_prompt = ?,
					updated_at = ?, version = version + 1
				WHERE tenant_id = ? AND permission_profile_id = ?
			`, profile.ID, profile.DailyLimit, profile.TotalQuota, profile.DailySpendingLimit,
				profile.PeriodSpendingLimits.FiveHour, profile.PeriodSpendingLimits.Week, profile.PeriodSpendingLimits.Month, profile.ConcurrencyLimit, profile.RPMLimit, profile.TPMLimit,
				mustJSONStringList(profile.AllowedModels), mustJSONStringList(profile.AllowedChannels),
				mustJSONStringList(profile.AllowedChannelGroups), profile.SystemPrompt,
				now, s.tenantID, profile.ID)
			if syncErr != nil {
				_ = tx.Rollback()
				return PermissionProfileSyncResult{}, syncErr
			}
			if rows, rowsErr := result.RowsAffected(); rowsErr == nil {
				appliedCount += rows
			}
		}

		unbindQuery := `
			UPDATE end_users SET permission_profile_id = '', updated_at = ?, version = version + 1
			WHERE tenant_id = ? AND permission_profile_id != ''`
		unbindArgs := []interface{}{now, s.tenantID}
		if len(seen) > 0 {
			placeholders := make([]string, 0, len(seen))
			for profileID := range seen {
				placeholders = append(placeholders, "?")
				unbindArgs = append(unbindArgs, profileID)
			}
			unbindQuery += ` AND permission_profile_id NOT IN (` + strings.Join(placeholders, ",") + `)`
		}
		result, syncErr := tx.Exec(unbindQuery, unbindArgs...)
		if syncErr != nil {
			_ = tx.Rollback()
			return PermissionProfileSyncResult{}, syncErr
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil {
			appliedCount += rows
		}
	}

	cappedKeys, err := capProfileOwnedKeysTx(tx, s.tenantID, profiles, now)
	if err != nil {
		_ = tx.Rollback()
		return PermissionProfileSyncResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PermissionProfileSyncResult{}, err
	}
	return PermissionProfileSyncResult{AppliedCount: appliedCount, CappedKeys: cappedKeys}, nil
}

func lockBoundEndUsersForProfileUpdate(tx *sql.Tx, tenantID string) error {
	query := `SELECT id FROM end_users WHERE tenant_id = ? AND permission_profile_id != '' ORDER BY id ASC FOR UPDATE`
	rows, err := tx.Query(query, tenantID)
	if err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "no such table") {
			return nil
		}
		if strings.Contains(message, "syntax") || strings.Contains(message, "for update") {
			rows, err = tx.Query(strings.TrimSuffix(query, " FOR UPDATE"), tenantID)
		}
	}
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil
		}
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func capProfileOwnedKeysTx(tx *sql.Tx, tenantID string, profiles []PermissionProfileRow, now string) ([]quota.CappedKey, error) {
	ceilings := make(map[string]quota.PeriodSpendingLimits, len(profiles))
	for _, profile := range profiles {
		profile = normalizePermissionProfile(profile)
		ceilings[profile.ID] = profile.PeriodSpendingLimits
	}
	rows, err := tx.Query(`SELECT k.id, e.permission_profile_id,
		COALESCE(k.daily_spending_limit,0), COALESCE(k.five_hour_spending_limit,0),
		COALESCE(k.weekly_spending_limit,0), COALESCE(k.monthly_spending_limit,0)
		FROM api_keys k JOIN end_users e ON e.id = k.end_user_id AND e.tenant_id = k.tenant_id
		WHERE k.tenant_id = ? AND e.permission_profile_id != '' ORDER BY k.id ASC`, tenantID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		id, profileID string
		limits        quota.PeriodSpendingLimits
	}
	items := make([]item, 0)
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.profileID, &v.limits.Day, &v.limits.FiveHour, &v.limits.Week, &v.limits.Month); err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, v)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]quota.CappedKey, 0)
	for _, v := range items {
		ceiling, ok := ceilings[v.profileID]
		if !ok {
			continue
		}
		updated := v.limits
		for _, period := range quota.OrderedPeriods {
			from, to := updated.Value(period), ceiling.Value(period)
			if from <= 0 || to <= 0 || from <= to {
				continue
			}
			switch period {
			case quota.PeriodFiveHour:
				updated.FiveHour = to
			case quota.PeriodDay:
				updated.Day = to
			case quota.PeriodWeek:
				updated.Week = to
			case quota.PeriodMonth:
				updated.Month = to
			}
			out = append(out, quota.CappedKey{ID: v.id, Period: period, From: from, To: to})
		}
		if updated != v.limits {
			if _, err := tx.Exec(`UPDATE api_keys SET daily_spending_limit=?, five_hour_spending_limit=?, weekly_spending_limit=?, monthly_spending_limit=?, updated_at=? WHERE tenant_id=? AND id=?`,
				updated.Day, updated.FiveHour, updated.Week, updated.Month, now, tenantID, v.id); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func normalizePermissionProfile(profile PermissionProfileRow) PermissionProfileRow {
	profile.ID = strings.TrimSpace(profile.ID)
	profile.Name = strings.TrimSpace(profile.Name)
	profile.DailyLimit = normalizeNonNegativeInt(profile.DailyLimit)
	profile.TotalQuota = normalizeNonNegativeInt(profile.TotalQuota)
	limits, err := quota.NormalizeLimits(profile.PeriodSpendingLimits)
	if err != nil {
		limits = quota.PeriodSpendingLimits{}
	}
	day, err := quota.NormalizeWholeUSD(profile.DailySpendingLimit)
	if err != nil {
		day = 0
	}
	if limits.Day > 0 && day == 0 {
		day = limits.Day
	}
	limits.Day = day
	profile.DailySpendingLimit = day
	profile.PeriodSpendingLimits = limits
	profile.ConcurrencyLimit = normalizeNonNegativeInt(profile.ConcurrencyLimit)
	profile.RPMLimit = normalizeNonNegativeInt(profile.RPMLimit)
	profile.TPMLimit = normalizeNonNegativeInt(profile.TPMLimit)
	profile.AllowedModels = normalizeStringSlice(profile.AllowedModels)
	profile.AllowedChannels = normalizeStringSlice(profile.AllowedChannels)
	profile.AllowedChannelGroups = normalizeStringSlice(profile.AllowedChannelGroups)
	profile.SystemPrompt = strings.TrimSpace(profile.SystemPrompt)
	return profile
}

func scanPermissionProfileRow(row scanner) (*PermissionProfileRow, bool) {
	var profile PermissionProfileRow
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&profile.ID, &profile.Name, &profile.DailyLimit, &profile.TotalQuota, &profile.DailySpendingLimit, &profile.PeriodSpendingLimits.FiveHour, &profile.PeriodSpendingLimits.Week, &profile.PeriodSpendingLimits.Month, &profile.ConcurrencyLimit,
		&profile.RPMLimit, &profile.TPMLimit, &modelsJSON, &channelsJSON, &channelGroupsJSON,
		&profile.SystemPrompt, &profile.CreatedAt, &profile.UpdatedAt,
	); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/apikey: scan api_key_permission_profiles row: %v", err)
		}
		return nil, false
	}
	profile.PeriodSpendingLimits.Day = profile.DailySpendingLimit
	profile.AllowedModels = decodeJSONStringList(modelsJSON)
	profile.AllowedChannels = decodeJSONStringList(channelsJSON)
	profile.AllowedChannelGroups = decodeJSONStringList(channelGroupsJSON)
	return &profile, true
}

func normalizeNonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// normalizeWholeUSD keeps spending limits as whole dollars (ceil partial input).
func normalizeWholeUSD(value float64) float64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Ceil(value)
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

package apikey

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	log "github.com/sirupsen/logrus"
)

const createAPIKeysTableSQL = `
CREATE TABLE IF NOT EXISTS api_keys (
  key               TEXT PRIMARY KEY NOT NULL,
  tenant_id         TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id                TEXT NOT NULL DEFAULT '',
  name              TEXT NOT NULL DEFAULT '',
  disabled          INTEGER NOT NULL DEFAULT 0,
  permission_profile_id TEXT NOT NULL DEFAULT '',
  daily_limit       INTEGER NOT NULL DEFAULT 0,
  total_quota       INTEGER NOT NULL DEFAULT 0,
  spending_limit    REAL NOT NULL DEFAULT 0,
  daily_spending_limit REAL NOT NULL DEFAULT 0,
  five_hour_spending_limit REAL NOT NULL DEFAULT 0,
  weekly_spending_limit REAL NOT NULL DEFAULT 0,
  monthly_spending_limit REAL NOT NULL DEFAULT 0,
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
	TenantID                string                     `json:"tenant_id,omitempty"`
	ID                      string                     `json:"id,omitempty"`
	Key                     string                     `json:"key"`
	Name                    string                     `json:"name,omitempty"`
	Disabled                bool                       `json:"disabled,omitempty"`
	EndUserID               string                     `json:"end_user_id,omitempty"`
	IsDefault               bool                       `json:"is_default,omitempty"`
	PermissionProfileID     string                     `json:"permission-profile-id,omitempty"`
	DailyLimit              int                        `json:"daily-limit,omitempty"`
	TotalQuota              int                        `json:"total-quota,omitempty"`
	SpendingLimit           float64                    `json:"spending-limit,omitempty"`
	DailySpendingLimit      float64                    `json:"daily-spending-limit,omitempty"`
	PeriodSpendingLimits    quota.PeriodSpendingLimits `json:"period-spending-limits"`
	PeriodSpending          []quota.PeriodSpending     `json:"period-spending,omitempty"`
	LifetimeSpendingUsed    float64                    `json:"lifetime-spending-used"`
	DailySpendingUsed       float64                    `json:"daily-spending-used"`
	DailySpendingResetCount int                        `json:"daily-spending-reset-count,omitempty"`
	ConcurrencyLimit        int                        `json:"concurrency-limit,omitempty"`
	RPMLimit                int                        `json:"rpm-limit,omitempty"`
	TPMLimit                int                        `json:"tpm-limit,omitempty"`
	AllowedModels           []string                   `json:"allowed-models,omitempty"`
	AllowedChannels         []string                   `json:"allowed-channels,omitempty"`
	AllowedChannelGroups    []string                   `json:"allowed-channel-groups,omitempty"`
	SystemPrompt            string                     `json:"system-prompt,omitempty"`
	CreatedAt               string                     `json:"created-at,omitempty"`
	UpdatedAt               string                     `json:"updated_at,omitempty"`
}

type PermissionProfileSnapshot struct {
	ID                   string
	DailyLimit           int
	TotalQuota           int
	DailySpendingLimit   float64
	PeriodSpendingLimits quota.PeriodSpendingLimits
	ConcurrencyLimit     int
	RPMLimit             int
	TPMLimit             int
	AllowedModels        []string
	AllowedChannels      []string
	AllowedChannelGroups []string
	SystemPrompt         string
}

type Store struct {
	db       *sql.DB
	tenantID string
}

type scanner interface {
	Scan(dest ...any) error
}

const systemTenantID = "00000000-0000-0000-0000-000000000001"

func NewStore(db *sql.DB) Store {
	return NewTenantStore(db, systemTenantID)
}

func NewTenantStore(db *sql.DB, tenantID string) Store {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = systemTenantID
	}
	return Store{db: db, tenantID: tenantID}
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
	// One-time repair: soft-delete used to leave the original secret in place.
	// Tombstone those rows so deleted keys cannot be re-enabled and reused.
	// Intentional disable (power toggle) is the same disabled=1 bit; after this
	// repair, only DELETE path tombstones. Operators who only disabled a key
	// lose re-enable-with-same-secret once — create a new key instead.
	tombstoneStaleDisabledOwnedSecrets(db)
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
		PeriodSpendingLimits: r.PeriodSpendingLimits,
		PeriodSpending:       r.PeriodSpending,
		LifetimeSpendingUsed: r.LifetimeSpendingUsed,
		ConcurrencyLimit:     r.ConcurrencyLimit,
		RPMLimit:             r.RPMLimit,
		TPMLimit:             r.TPMLimit,
		AllowedModels:        r.AllowedModels,
		AllowedChannels:      r.AllowedChannels,
		AllowedChannelGroups: r.AllowedChannelGroups,
		SystemPrompt:         r.SystemPrompt,
		CreatedAt:            r.CreatedAt,
		EndUserID:            r.EndUserID,
		IsDefault:            r.IsDefault,
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
		PeriodSpendingLimits: entry.PeriodSpendingLimits,
		ConcurrencyLimit:     entry.ConcurrencyLimit,
		RPMLimit:             entry.RPMLimit,
		TPMLimit:             entry.TPMLimit,
		AllowedModels:        entry.AllowedModels,
		AllowedChannels:      entry.AllowedChannels,
		AllowedChannelGroups: entry.AllowedChannelGroups,
		SystemPrompt:         entry.SystemPrompt,
		CreatedAt:            entry.CreatedAt,
		EndUserID:            entry.EndUserID,
		IsDefault:            entry.IsDefault,
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
	if err := s.db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE tenant_id = ?", s.tenantID).Scan(&count); err != nil {
		log.Warnf("sqlite/apikey: count api_keys: %v", err)
		return 0
	}
	return count
}

func (s Store) ListAll() []APIKeyRow {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`SELECT tenant_id, key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys ORDER BY tenant_id ASC, created_at ASC`)
	if err != nil {
		log.Errorf("sqlite/apikey: list all api_keys: %v", err)
		return nil
	}
	defer rows.Close()

	result := make([]APIKeyRow, 0)
	for rows.Next() {
		entry, ok := scanAPIKeyRowWithTenant(rows)
		if ok {
			result = append(result, *entry)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("sqlite/apikey: scan all api_keys rows: %v", err)
	}
	return result
}

func (s Store) List() []APIKeyRow {
	if s.db == nil {
		return nil
	}

	rows, err := s.db.Query(`SELECT key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE tenant_id = ? ORDER BY created_at ASC`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/apikey: list api_keys: %v", err)
		return nil
	}
	defer rows.Close()

	items := scanAPIKeyRows(rows)
	for i := range items {
		items[i].TenantID = s.tenantID
	}
	return items
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
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE tenant_id = ? AND key = ?`, s.tenantID, trimmed)
	entry, ok := scanAPIKeyRow(row)
	if !ok {
		return nil
	}
	entry.TenantID = s.tenantID
	return entry
}

// GetAnyTenant resolves a globally unique API key secret without assuming the
// system tenant. Public authentication starts from the presented secret, so
// tenant scope can only be applied after this lookup succeeds.
func (s Store) GetAnyTenant(key string) *APIKeyRow {
	if s.db == nil {
		return nil
	}

	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return nil
	}

	row := s.db.QueryRow(`SELECT tenant_id, key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE key = ?`, trimmed)
	entry, ok := scanAPIKeyRowWithTenant(row)
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
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE tenant_id = ? AND id = ?`, s.tenantID, trimmed)
	entry, ok := scanAPIKeyRow(row)
	if !ok {
		return nil
	}
	entry.TenantID = s.tenantID
	return entry
}

// GetByIDAnyTenant resolves a globally unique stable key id without assuming
// the system tenant.
func (s Store) GetByIDAnyTenant(id string) *APIKeyRow {
	if s.db == nil {
		return nil
	}

	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil
	}

	row := s.db.QueryRow(`SELECT tenant_id, key, name, disabled, id, daily_limit, total_quota,
		permission_profile_id, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit, concurrency_limit, rpm_limit, tpm_limit,
		allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		end_user_id, is_default
		FROM api_keys WHERE id = ?`, trimmed)
	entry, ok := scanAPIKeyRowWithTenant(row)
	if !ok {
		return nil
	}
	return entry
}

// stripOwnedKeyAccountManagedFields clears fields owned keys inherit from their account.
// The four period spending limits remain key-scoped sub-quotas.
func stripOwnedKeyAccountManagedFields(entry *APIKeyRow) {
	if entry == nil || strings.TrimSpace(entry.EndUserID) == "" {
		return
	}
	entry.PermissionProfileID = ""
	entry.DailyLimit = 0
	entry.TotalQuota = 0
	entry.SpendingLimit = 0
	entry.ConcurrencyLimit = 0
	entry.RPMLimit = 0
	entry.TPMLimit = 0
	entry.AllowedModels = nil
	entry.AllowedChannels = nil
	entry.AllowedChannelGroups = nil
	entry.SystemPrompt = ""
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
	// Preserve ownership if caller omitted end_user_id, then force account-level quota semantics.
	if strings.TrimSpace(entry.EndUserID) == "" {
		if existing := s.GetByID(entry.ID); existing != nil && existing.EndUserID != "" {
			entry.EndUserID = existing.EndUserID
		} else if existing := s.Get(entry.Key); existing != nil && existing.EndUserID != "" {
			entry.EndUserID = existing.EndUserID
		}
	}
	stripOwnedKeyAccountManagedFields(&entry)

	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}

	disabledInt := 0
	if entry.Disabled {
		disabledInt = 1
	}
	// Pass bool for is_default: Postgres column is BOOLEAN; SQLite accepts 0/1 via bool too.
	isDefault := entry.IsDefault
	var endUserID any
	if strings.TrimSpace(entry.EndUserID) != "" {
		endUserID = strings.TrimSpace(entry.EndUserID)
	}

	result, err := s.db.Exec(`INSERT INTO api_keys
		(tenant_id, key, id, name, disabled, permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		 end_user_id, is_default)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			id=excluded.id,
			name=excluded.name, disabled=excluded.disabled,
			permission_profile_id=excluded.permission_profile_id,
			daily_limit=excluded.daily_limit, total_quota=excluded.total_quota,
			spending_limit=excluded.spending_limit, daily_spending_limit=excluded.daily_spending_limit,
			five_hour_spending_limit=excluded.five_hour_spending_limit, weekly_spending_limit=excluded.weekly_spending_limit, monthly_spending_limit=excluded.monthly_spending_limit,
			concurrency_limit=excluded.concurrency_limit,
			rpm_limit=excluded.rpm_limit, tpm_limit=excluded.tpm_limit,
			allowed_models=excluded.allowed_models, allowed_channels=excluded.allowed_channels,
			allowed_channel_groups=excluded.allowed_channel_groups,
			system_prompt=excluded.system_prompt,
			end_user_id=COALESCE(excluded.end_user_id, api_keys.end_user_id),
			is_default=CASE WHEN excluded.end_user_id IS NOT NULL THEN excluded.is_default ELSE api_keys.is_default END,
			updated_at=excluded.updated_at
		WHERE api_keys.tenant_id = excluded.tenant_id`,
		s.tenantID, entry.Key, entry.ID, entry.Name, disabledInt, entry.PermissionProfileID,
		entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit, entry.DailySpendingLimit,
		entry.PeriodSpendingLimits.FiveHour, entry.PeriodSpendingLimits.Week, entry.PeriodSpendingLimits.Month, entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
		mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
		mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
		entry.CreatedAt, now, endUserID, isDefault,
	)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("api key already belongs to another tenant")
	}
	return nil
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

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var owner sql.NullString
	var createdAt string
	var wasDefault bool
	if err = tx.QueryRow(`
		SELECT end_user_id, created_at, COALESCE(is_default, false)
		FROM api_keys WHERE tenant_id = ? AND id = ?
	`, s.tenantID, entry.ID).Scan(&owner, &createdAt, &wasDefault); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	ownerID := ""
	if owner.Valid {
		ownerID = strings.TrimSpace(owner.String)
	}
	if ownerID != "" {
		if err = lockOwnedEndUser(tx, ownerID); err != nil {
			return err
		}
		if err = tx.QueryRow(`SELECT COALESCE(daily_spending_limit,0), COALESCE(five_hour_spending_limit,0), COALESCE(weekly_spending_limit,0), COALESCE(monthly_spending_limit,0) FROM api_keys WHERE tenant_id = ? AND id = ?`, s.tenantID, entry.ID).
			Scan(&entry.PeriodSpendingLimits.Day, &entry.PeriodSpendingLimits.FiveHour, &entry.PeriodSpendingLimits.Week, &entry.PeriodSpendingLimits.Month); err != nil {
			return err
		}
		entry.DailySpendingLimit = entry.PeriodSpendingLimits.Day
		entry.EndUserID = ownerID
		entry.IsDefault = wasDefault && !entry.Disabled
	} else {
		entry.EndUserID = ""
		entry.IsDefault = false
	}
	if entry.CreatedAt == "" {
		entry.CreatedAt = createdAt
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}
	stripOwnedKeyAccountManagedFields(&entry)

	disabledInt := 0
	if entry.Disabled {
		disabledInt = 1
	}
	result, err := tx.Exec(`UPDATE api_keys SET
		key = ?, name = ?, disabled = ?, permission_profile_id = ?, daily_limit = ?, total_quota = ?,
		spending_limit = ?, daily_spending_limit = ?, five_hour_spending_limit = ?, weekly_spending_limit = ?, monthly_spending_limit = ?, concurrency_limit = ?, rpm_limit = ?, tpm_limit = ?,
		allowed_models = ?, allowed_channels = ?, allowed_channel_groups = ?, system_prompt = ?,
		created_at = ?, updated_at = ?, is_default = ?
		WHERE tenant_id = ? AND id = ?`,
		entry.Key, entry.Name, disabledInt, entry.PermissionProfileID, entry.DailyLimit, entry.TotalQuota,
		entry.SpendingLimit, entry.DailySpendingLimit, entry.PeriodSpendingLimits.FiveHour, entry.PeriodSpendingLimits.Week, entry.PeriodSpendingLimits.Month,
		entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
		mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
		mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
		entry.CreatedAt, now, entry.IsDefault, s.tenantID, entry.ID,
	)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil
	}
	if ownerID != "" {
		if err = ensureOwnedActiveKeyAndDefault(tx, s.tenantID, ownerID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func lockOwnedEndUser(tx *sql.Tx, ownerID string) error {
	if tx == nil || strings.TrimSpace(ownerID) == "" {
		return nil
	}
	if _, err := tx.Exec(`SELECT id FROM end_users WHERE id = ? FOR UPDATE`, ownerID); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "syntax") || strings.Contains(msg, "for update") {
			return nil
		}
		return err
	}
	return nil
}

func ensureOwnedActiveKeyAndDefault(tx *sql.Tx, tenantID, ownerID, now string) error {
	var activeCount int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM api_keys
		WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0
	`, tenantID, ownerID).Scan(&activeCount); err != nil {
		return err
	}
	if activeCount < 1 {
		return fmt.Errorf("cannot disable last active api key for end user %s", ownerID)
	}
	if _, err := tx.Exec(`
		UPDATE api_keys SET is_default = false, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ? AND disabled != 0 AND is_default
	`, now, tenantID, ownerID); err != nil {
		return err
	}

	var keepID string
	err := tx.QueryRow(`
		SELECT id FROM api_keys
		WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0 AND is_default
		ORDER BY created_at ASC, id ASC LIMIT 1
	`, tenantID, ownerID).Scan(&keepID)
	if err == sql.ErrNoRows {
		if err = tx.QueryRow(`
			SELECT id FROM api_keys
			WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0
			ORDER BY created_at ASC, id ASC LIMIT 1
		`, tenantID, ownerID).Scan(&keepID); err != nil {
			return err
		}
		if _, err = tx.Exec(`
			UPDATE api_keys SET is_default = true, updated_at = ?
			WHERE tenant_id = ? AND id = ?
		`, now, tenantID, keepID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = tx.Exec(`
		UPDATE api_keys SET is_default = false, updated_at = ?
		WHERE tenant_id = ? AND end_user_id = ? AND disabled = 0 AND is_default AND id <> ?
	`, now, tenantID, ownerID, keepID)
	return err
}

func (s Store) deleteOwnedGuarded(where string, arg string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return fmt.Errorf("key is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var endUserID sql.NullString
	var keyID string
	var disabledInt int
	q := `SELECT id, end_user_id, disabled FROM api_keys WHERE tenant_id = ? AND ` + where + ` LIMIT 1`
	if err = tx.QueryRow(q, s.tenantID, arg).Scan(&keyID, &endUserID, &disabledInt); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	ownerID := ""
	if endUserID.Valid {
		ownerID = strings.TrimSpace(endUserID.String)
	}
	if ownerID != "" {
		if err = lockOwnedEndUser(tx, ownerID); err != nil {
			return err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if ownerID != "" {
		// Soft-delete keeps the row (stable id → request_logs ownership) but
		// permanently invalidates the secret so the deleted key can never auth
		// again, including via accidental re-enable. Also covers "disabled then
		// deleted" so a prior toggle-disable cannot leave a reusable secret.
		tombstone, genErr := newTombstoneSecret()
		if genErr != nil {
			return genErr
		}
		if _, err = tx.Exec(`
			UPDATE api_keys SET key = ?, disabled = 1, is_default = false, updated_at = ?
			WHERE tenant_id = ? AND id = ?
		`, tombstone, now, s.tenantID, keyID); err != nil {
			return err
		}
		if err = ensureOwnedActiveKeyAndDefault(tx, s.tenantID, ownerID, now); err != nil {
			return err
		}
	} else if _, err = tx.Exec(`DELETE FROM api_keys WHERE tenant_id = ? AND id = ?`, s.tenantID, keyID); err != nil {
		return err
	}
	return tx.Commit()
}

// newTombstoneSecret returns a unique unusable secret for soft-deleted owned keys.
// api_keys.key is globally UNIQUE, so each tombstone must differ.
func newTombstoneSecret() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "sk-deleted-" + hex.EncodeToString(raw), nil
}

func (s Store) Delete(key string) error {
	return s.deleteOwnedGuarded("key = ?", key)
}

func (s Store) DeleteByID(id string) error {
	return s.deleteOwnedGuarded("id = ?", id)
}

func (s Store) ReplaceAll(entries []APIKeyRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	// Preserve end-user ownership across full replace.
	// Prefer stable id, then key text (admin may rename key secret while keeping id).
	type ownership struct {
		id        string
		key       string
		endUserID string
		isDefault bool
	}
	byID := make(map[string]ownership)
	byKey := make(map[string]ownership)
	for _, row := range s.List() {
		key := strings.TrimSpace(row.Key)
		id := strings.TrimSpace(row.ID)
		own := ownership{
			id:        id,
			key:       key,
			endUserID: strings.TrimSpace(row.EndUserID),
			isDefault: row.IsDefault,
		}
		if id != "" {
			byID[id] = own
		}
		if key != "" {
			byKey[key] = own
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}

	// Resolve ownership for each incoming row first, then verify every previously
	// owned end user still ends up with >=1 key. Counting by ID and key separately
	// is unsafe: id=A+key=B would mark both A and B as "kept" while only A survives.
	type resolvedEntry struct {
		row APIKeyRow
	}
	resolved := make([]resolvedEntry, 0, len(entries))
	seenIDs := make(map[string]struct{}, len(entries))
	seenKeys := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry = normalizeRow(entry)
		if entry.Key == "" {
			continue
		}
		if _, dup := seenKeys[entry.Key]; dup {
			_ = tx.Rollback()
			return fmt.Errorf("duplicate api key in replace payload")
		}
		seenKeys[entry.Key] = struct{}{}
		prev, ok := byID[entry.ID]
		if !ok {
			prev = byKey[entry.Key]
		}
		if entry.ID == "" {
			if prev.id != "" {
				entry.ID = prev.id
			} else {
				entry.ID = uuid.NewString()
			}
		}
		if _, dup := seenIDs[entry.ID]; dup {
			_ = tx.Rollback()
			return fmt.Errorf("duplicate api key id in replace payload")
		}
		seenIDs[entry.ID] = struct{}{}
		// Ownership is not client-authoritative on full replace for existing keys.
		if prev.endUserID != "" {
			entry.EndUserID = prev.endUserID
			entry.IsDefault = prev.isDefault
		} else {
			// Brand-new keys may not carry ownership through generic replace.
			entry.EndUserID = ""
			entry.IsDefault = false
		}
		resolved = append(resolved, resolvedEntry{row: entry})
	}
	ownedBefore := make(map[string]struct{})
	for _, prev := range byID {
		if prev.endUserID != "" {
			ownedBefore[prev.endUserID] = struct{}{}
		}
	}
	for _, prev := range byKey {
		if prev.endUserID != "" {
			ownedBefore[prev.endUserID] = struct{}{}
		}
	}
	if _, err := tx.Exec("DELETE FROM api_keys WHERE tenant_id = ?", s.tenantID); err != nil {
		_ = tx.Rollback()
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO api_keys
		(tenant_id, key, id, name, disabled, permission_profile_id, daily_limit, total_quota, spending_limit, daily_spending_limit, five_hour_spending_limit, weekly_spending_limit, monthly_spending_limit,
		 concurrency_limit, rpm_limit, tpm_limit, allowed_models, allowed_channels, allowed_channel_groups, system_prompt, created_at, updated_at,
		 end_user_id, is_default)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, item := range resolved {
		entry := item.row
		if entry.CreatedAt == "" {
			entry.CreatedAt = now
		}
		disabledInt := 0
		if entry.Disabled {
			disabledInt = 1
		}
		isDefault := entry.IsDefault
		var endUserID any
		if entry.EndUserID != "" {
			endUserID = entry.EndUserID
		}
		if _, err := stmt.Exec(
			s.tenantID, entry.Key, entry.ID, entry.Name, disabledInt, entry.PermissionProfileID,
			entry.DailyLimit, entry.TotalQuota, entry.SpendingLimit, entry.DailySpendingLimit,
			entry.PeriodSpendingLimits.FiveHour, entry.PeriodSpendingLimits.Week, entry.PeriodSpendingLimits.Month,
			entry.ConcurrencyLimit, entry.RPMLimit, entry.TPMLimit,
			mustJSONStringList(entry.AllowedModels), mustJSONStringList(entry.AllowedChannels),
			mustJSONStringList(entry.AllowedChannelGroups), entry.SystemPrompt,
			entry.CreatedAt, now, endUserID, isDefault,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	for endUserID := range ownedBefore {
		if err := ensureOwnedActiveKeyAndDefault(tx, s.tenantID, endUserID, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

func migrateColumns(db *sql.DB) {
	for _, col := range []struct {
		name       string
		definition string
	}{
		{name: "tenant_id", definition: "TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"},
		{name: "id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "permission_profile_id", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "allowed_channels", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{name: "allowed_channel_groups", definition: "TEXT NOT NULL DEFAULT '[]'"},
		{name: "daily_spending_limit", definition: "REAL NOT NULL DEFAULT 0"},
		{name: "five_hour_spending_limit", definition: "REAL NOT NULL DEFAULT 0"},
		{name: "weekly_spending_limit", definition: "REAL NOT NULL DEFAULT 0"},
		{name: "monthly_spending_limit", definition: "REAL NOT NULL DEFAULT 0"},
		{name: "end_user_id", definition: "TEXT"},
		{name: "is_default", definition: "INTEGER NOT NULL DEFAULT 0"},
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

// tombstoneStaleDisabledOwnedSecrets invalidates secrets for already soft-deleted
// owned keys that still hold their original plaintext (pre-fix rows).
func tombstoneStaleDisabledOwnedSecrets(db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.Query(`
		SELECT id FROM api_keys
		WHERE disabled != 0
		  AND end_user_id IS NOT NULL AND trim(end_user_id) != ''
		  AND key NOT LIKE 'sk-deleted-%'
	`)
	if err != nil {
		log.Warnf("sqlite/apikey: list stale disabled owned secrets: %v", err)
		return
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			log.Warnf("sqlite/apikey: scan stale disabled owned secret: %v", err)
			return
		}
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		log.Warnf("sqlite/apikey: iterate stale disabled owned secrets: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	fixed := 0
	for _, id := range ids {
		tombstone, genErr := newTombstoneSecret()
		if genErr != nil {
			log.Warnf("sqlite/apikey: tombstone secret: %v", genErr)
			return
		}
		res, execErr := db.Exec(`
			UPDATE api_keys SET key = ?, is_default = false, updated_at = ?
			WHERE id = ? AND disabled != 0 AND key NOT LIKE 'sk-deleted-%'
		`, tombstone, now, id)
		if execErr != nil {
			log.Warnf("sqlite/apikey: tombstone owned secret id=%s: %v", id, execErr)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fixed++
		}
	}
	if fixed > 0 {
		log.Infof("sqlite/apikey: invalidated secrets for %d soft-deleted owned keys", fixed)
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
	row.EndUserID = strings.TrimSpace(row.EndUserID)
	row.PermissionProfileID = strings.TrimSpace(row.PermissionProfileID)
	row.SpendingLimit = normalizeWholeUSD(row.SpendingLimit)
	limits, err := quota.NormalizeLimits(row.PeriodSpendingLimits)
	if err != nil {
		limits = quota.PeriodSpendingLimits{}
	}
	day, err := quota.NormalizeWholeUSD(row.DailySpendingLimit)
	if err != nil {
		day = 0
	}
	if limits.Day > 0 && day == 0 {
		day = limits.Day
	}
	limits.Day = day
	row.DailySpendingLimit = day
	row.PeriodSpendingLimits = limits
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

func scanAPIKeyRowWithTenant(row scanner) (*APIKeyRow, bool) {
	var entry APIKeyRow
	var disabledInt int
	var endUserID sql.NullString
	var isDefault any
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&entry.TenantID, &entry.Key, &entry.Name, &disabledInt,
		&entry.ID,
		&entry.DailyLimit, &entry.TotalQuota, &entry.PermissionProfileID, &entry.SpendingLimit,
		&entry.DailySpendingLimit, &entry.PeriodSpendingLimits.FiveHour, &entry.PeriodSpendingLimits.Week, &entry.PeriodSpendingLimits.Month, &entry.ConcurrencyLimit, &entry.RPMLimit, &entry.TPMLimit,
		&modelsJSON, &channelsJSON, &channelGroupsJSON, &entry.SystemPrompt,
		&entry.CreatedAt, &entry.UpdatedAt, &endUserID, &isDefault,
	); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/apikey: scan tenant api_keys row: %v", err)
		}
		return nil, false
	}
	if endUserID.Valid {
		entry.EndUserID = endUserID.String
	}
	entry.IsDefault = boolish(isDefault)
	entry.PeriodSpendingLimits.Day = entry.DailySpendingLimit
	decodeAPIKeyRow(&entry, disabledInt, modelsJSON, channelsJSON, channelGroupsJSON)
	return &entry, true
}

func scanAPIKeyRow(row scanner) (*APIKeyRow, bool) {
	var entry APIKeyRow
	var disabledInt int
	var endUserID sql.NullString
	var isDefault any
	var modelsJSON string
	var channelsJSON string
	var channelGroupsJSON string
	if err := row.Scan(
		&entry.Key, &entry.Name, &disabledInt,
		&entry.ID,
		&entry.DailyLimit, &entry.TotalQuota, &entry.PermissionProfileID, &entry.SpendingLimit,
		&entry.DailySpendingLimit, &entry.PeriodSpendingLimits.FiveHour, &entry.PeriodSpendingLimits.Week, &entry.PeriodSpendingLimits.Month, &entry.ConcurrencyLimit, &entry.RPMLimit, &entry.TPMLimit,
		&modelsJSON, &channelsJSON, &channelGroupsJSON, &entry.SystemPrompt,
		&entry.CreatedAt, &entry.UpdatedAt, &endUserID, &isDefault,
	); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/apikey: scan api_keys row: %v", err)
		}
		return nil, false
	}
	if endUserID.Valid {
		entry.EndUserID = endUserID.String
	}
	entry.IsDefault = boolish(isDefault)
	entry.PeriodSpendingLimits.Day = entry.DailySpendingLimit
	decodeAPIKeyRow(&entry, disabledInt, modelsJSON, channelsJSON, channelGroupsJSON)
	return &entry, true
}

// boolish accepts Postgres bool and SQLite integer 0/1 for is_default.
func boolish(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int32:
		return t != 0
	case int:
		return t != 0
	case []byte:
		if len(t) == 1 {
			return t[0] != 0 && t[0] != '0'
		}
		return string(t) == "true" || string(t) == "t" || string(t) == "1"
	case string:
		return t == "true" || t == "t" || t == "1"
	default:
		return false
	}
}

func decodeAPIKeyRow(entry *APIKeyRow, disabledInt int, modelsJSON, channelsJSON, channelGroupsJSON string) {
	entry.Disabled = disabledInt != 0
	entry.TenantID = strings.TrimSpace(entry.TenantID)
	entry.EndUserID = strings.TrimSpace(entry.EndUserID)
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
}

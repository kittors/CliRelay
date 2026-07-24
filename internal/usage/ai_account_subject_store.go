package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	AIAccountStatusScopeShared  = "shared_subject"
	AIAccountSubjectScopeShared = "shared"
	AIAccountSubjectScopeTenant = "tenant"

	aiAccountSharedSchemaMarker   = "ai_account_shared_subject_schema_v1"
	aiAccountSharedBackfillMarker = "ai_account_shared_subject_backfill_v1"
	aiAccountSharedReadMarker     = "ai_account_shared_subject_read_v1"
	aiAccountSharedOldWriteMarker = "ai_account_shared_subject_old_write_v1"
)

// SQLite mirrors the PostgreSQL migration semantics. The high-frequency usage
// table deliberately has no subject FK so request logging cannot fail because a
// low-frequency binding reconciliation was temporarily unavailable.
const aiAccountSharedSubjectTablesSQL = `
CREATE TABLE IF NOT EXISTS ai_account_subjects (
  auth_subject_id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  subject_scope TEXT NOT NULL CHECK (subject_scope IN ('shared', 'tenant')),
  seed_kind TEXT NOT NULL,
  seed_hash TEXT NOT NULL,
  share_eligible INTEGER NOT NULL DEFAULT 0,
  usage_projected_since DATETIME,
  usage_history_complete INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  UNIQUE (provider, subject_scope, seed_kind, seed_hash)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subjects_provider_scope
  ON ai_account_subjects(provider, subject_scope, updated_at);
CREATE TABLE IF NOT EXISTS ai_account_tenant_bindings (
  tenant_id TEXT NOT NULL,
  auth_id TEXT NOT NULL,
  auth_index TEXT NOT NULL,
  provider TEXT NOT NULL,
  auth_subject_id TEXT NOT NULL,
  binding_seed_kind TEXT NOT NULL,
  binding_seed_hash TEXT NOT NULL,
  share_eligible INTEGER NOT NULL DEFAULT 0,
  binding_state TEXT NOT NULL DEFAULT 'active' CHECK (binding_state IN ('active', 'deleted')),
  binding_revision INTEGER NOT NULL DEFAULT 1,
  bound_at DATETIME NOT NULL,
  last_seen_at DATETIME NOT NULL,
  deleted_at DATETIME,
  PRIMARY KEY (tenant_id, auth_id),
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_account_binding_active_index
  ON ai_account_tenant_bindings(tenant_id, auth_index) WHERE binding_state = 'active';
CREATE INDEX IF NOT EXISTS idx_ai_account_binding_subject
  ON ai_account_tenant_bindings(auth_subject_id, binding_state);
CREATE INDEX IF NOT EXISTS idx_ai_account_binding_tenant_subject
  ON ai_account_tenant_bindings(tenant_id, auth_subject_id, binding_state);
CREATE TABLE IF NOT EXISTS ai_account_subject_status (
  auth_subject_id TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  last_probe_state TEXT NOT NULL DEFAULT 'idle' CHECK (last_probe_state IN ('idle', 'success', 'error')),
  health_status TEXT NOT NULL DEFAULT '',
  plan_type TEXT NOT NULL DEFAULT '',
  subscription_started_at DATETIME,
  subscription_expires_at DATETIME,
  subscription_source TEXT NOT NULL DEFAULT '' CHECK (subscription_source IN ('', 'probe', 'signed_claims', 'migration')),
  restriction_summary TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  error_summary TEXT NOT NULL DEFAULT '',
  quota_json TEXT NOT NULL DEFAULT '[]',
  reset_credit_count INTEGER,
  reset_credit_expirations TEXT NOT NULL DEFAULT '[]',
  upstream_checked_at DATETIME,
  version INTEGER NOT NULL DEFAULT 1,
  updated_at DATETIME NOT NULL,
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_status_checked
  ON ai_account_subject_status(upstream_checked_at, updated_at);
CREATE TABLE IF NOT EXISTS ai_account_subject_usage_buckets (
  auth_subject_id TEXT NOT NULL,
  bucket_kind TEXT NOT NULL CHECK (bucket_kind IN ('day', 'lifetime', 'cycle')),
  bucket_start TEXT NOT NULL,
  request_count INTEGER NOT NULL DEFAULT 0,
  success_count INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  cost_total REAL NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  first_event_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL,
  PRIMARY KEY (auth_subject_id, bucket_kind, bucket_start)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_usage_day
  ON ai_account_subject_usage_buckets(bucket_kind, bucket_start, auth_subject_id);
CREATE TABLE IF NOT EXISTS ai_account_subject_quota_cycles (
  auth_subject_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  quota_key TEXT NOT NULL,
  cycle_start_at DATETIME NOT NULL,
  reset_at DATETIME NOT NULL,
  window_seconds INTEGER NOT NULL DEFAULT 0,
  last_verified_at DATETIME NOT NULL,
  PRIMARY KEY (auth_subject_id, quota_key),
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE TABLE IF NOT EXISTS ai_account_subject_quota_points (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  auth_subject_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  quota_key TEXT NOT NULL,
  quota_label TEXT NOT NULL DEFAULT '',
  percent REAL,
  reset_at DATETIME,
  window_seconds INTEGER NOT NULL DEFAULT 0,
  recorded_at DATETIME NOT NULL,
  FOREIGN KEY (auth_subject_id) REFERENCES ai_account_subjects(auth_subject_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_account_subject_quota_points_key_time
  ON ai_account_subject_quota_points(auth_subject_id, quota_key, recorded_at DESC);
`

type AIAccountSubjectRecord struct {
	AuthSubjectID        string
	Provider             string
	SubjectScope         string
	SeedKind             string
	SeedHash             string
	ShareEligible        bool
	UsageProjectedSince  *time.Time
	UsageHistoryComplete bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type AIAccountTenantBinding struct {
	TenantID        string
	AuthID          string
	AuthIndex       string
	Provider        string
	AuthSubjectID   string
	BindingSeedKind string
	BindingSeedHash string
	ShareEligible   bool
	BindingState    string
	BindingRevision int64
	BoundAt         time.Time
	LastSeenAt      time.Time
	DeletedAt       *time.Time
}

type AIAccountSubjectStatusRecord struct {
	AuthSubjectID          string
	Provider               string
	LastProbeState         string
	HealthStatus           string
	PlanType               string
	SubscriptionStartedAt  *time.Time
	SubscriptionExpiresAt  *time.Time
	SubscriptionSource     string
	RestrictionSummary     string
	ErrorCode              string
	ErrorSummary           string
	Quotas                 []QuotaWindowDTO
	ResetCreditCount       *int64
	ResetCreditExpirations []string
	UpstreamCheckedAt      *time.Time
	Version                int64
	UpdatedAt              time.Time
}

type AIAccountSharedBackfillReport struct {
	Subjects               int64
	Bindings               int64
	StatusRows             int64
	UsageRows              int64
	QuotaCycles            int64
	QuotaPoints            int64
	UnboundLegacySubjects  int64
	StatusPayloadConflicts int64
	Checksum               string
}

func ensureAIAccountSharedSubjectTables(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if usageDriver != "postgres" {
		if _, err := db.Exec(aiAccountSharedSubjectTablesSQL); err != nil {
			return fmt.Errorf("usage: ensure ai account shared subject tables: %w", err)
		}
		// Additive migration for SQLite databases created before cycle token projection.
		_, _ = db.Exec(`ALTER TABLE ai_account_subject_usage_buckets ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`)
	}
	ensureUsageProjectionMarkerTable(db)
	if err := setProjectionMarker(db, aiAccountSharedSchemaMarker, "done"); err != nil {
		return err
	}
	for key, value := range map[string]string{
		aiAccountSharedReadMarker:     "shared",
		aiAccountSharedOldWriteMarker: "enabled",
	} {
		if projectionMarkerValue(db, key) == "" {
			if err := setProjectionMarker(db, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func UpsertAIAccountSubject(identity *AuthSubjectIdentity) error {
	if identity == nil || strings.TrimSpace(identity.ID) == "" {
		return nil
	}
	db := getDB()
	if db == nil {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Use bool (not integer 0/1): Postgres column is BOOLEAN; integer literals fail with SQLSTATE 42804.
	_, err := db.Exec(`
		INSERT INTO ai_account_subjects (
			auth_subject_id, provider, subject_scope, seed_kind, seed_hash,
			share_eligible, usage_history_complete, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			provider = excluded.provider,
			subject_scope = excluded.subject_scope,
			seed_kind = excluded.seed_kind,
			seed_hash = excluded.seed_hash,
			share_eligible = excluded.share_eligible,
			updated_at = excluded.updated_at
	`, strings.TrimSpace(identity.ID), strings.TrimSpace(identity.Provider), identity.SubjectScope,
		identity.SeedKind, identity.SeedHash, identity.ShareEligible, false, now, now)
	if err != nil {
		return fmt.Errorf("usage: upsert ai account subject: %w", err)
	}
	return nil
}

func UpsertAIAccountTenantBinding(auth *coreauth.Auth, identity *AuthSubjectIdentity) error {
	if auth == nil || identity == nil || strings.TrimSpace(auth.ID) == "" || strings.TrimSpace(identity.ID) == "" {
		return nil
	}
	if err := UpsertAIAccountSubject(identity); err != nil {
		return err
	}
	db := getDB()
	if db == nil {
		return nil
	}
	tenantID := normalizeTenantID(auth.TenantID)
	authID := strings.TrimSpace(auth.ID)
	authIndex := strings.TrimSpace(auth.EnsureIndex())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Partial unique index (tenant_id, auth_index) WHERE active: free the index slot first.
	if authIndex != "" {
		if _, err := db.Exec(`
			UPDATE ai_account_tenant_bindings
			SET binding_state = 'deleted',
			    binding_revision = binding_revision + 1,
			    deleted_at = ?,
			    last_seen_at = ?
			WHERE tenant_id = ? AND auth_index = ? AND binding_state = 'active' AND auth_id <> ?
		`, now, now, tenantID, authIndex, authID); err != nil {
			return fmt.Errorf("usage: free active auth_index binding: %w", err)
		}
	}
	_, err := db.Exec(`
		INSERT INTO ai_account_tenant_bindings (
			tenant_id, auth_id, auth_index, provider, auth_subject_id,
			binding_seed_kind, binding_seed_hash, share_eligible,
			binding_state, binding_revision, bound_at, last_seen_at, deleted_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', 1, ?, ?, NULL)
		ON CONFLICT(tenant_id, auth_id) DO UPDATE SET
			auth_index = excluded.auth_index,
			provider = excluded.provider,
			auth_subject_id = excluded.auth_subject_id,
			binding_seed_kind = excluded.binding_seed_kind,
			binding_seed_hash = excluded.binding_seed_hash,
			share_eligible = excluded.share_eligible,
			binding_state = 'active',
			binding_revision = CASE
				WHEN ai_account_tenant_bindings.auth_subject_id <> excluded.auth_subject_id
				  OR ai_account_tenant_bindings.binding_state <> 'active'
				THEN ai_account_tenant_bindings.binding_revision + 1
				ELSE ai_account_tenant_bindings.binding_revision
			END,
			last_seen_at = excluded.last_seen_at,
			deleted_at = NULL
	`, tenantID, authID, authIndex, identity.Provider, identity.ID,
		identity.SeedKind, identity.SeedHash, identity.ShareEligible, now, now)
	if err != nil {
		return fmt.Errorf("usage: upsert ai account tenant binding: %w", err)
	}
	return nil
}

func MarkAIAccountTenantBindingDeleted(tenantID, authID string) error {
	db := getDB()
	if db == nil || strings.TrimSpace(authID) == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`
		UPDATE ai_account_tenant_bindings
		SET binding_state = 'deleted', binding_revision = binding_revision + 1,
			deleted_at = ?, last_seen_at = ?
		WHERE tenant_id = ? AND auth_id = ? AND binding_state <> 'deleted'
	`, now, now, normalizeTenantID(tenantID), strings.TrimSpace(authID))
	if err != nil {
		return fmt.Errorf("usage: mark ai account binding deleted: %w", err)
	}
	return nil
}

func ListAIAccountBindingsForTenantAuths(tenantID string, authIDs []string) ([]AIAccountTenantBinding, error) {
	db := getReadDB()
	if db == nil {
		return []AIAccountTenantBinding{}, nil
	}
	ids := dedupeExactStrings(authIDs)
	if len(ids) == 0 {
		return []AIAccountTenantBinding{}, nil
	}
	args := []any{normalizeTenantID(tenantID)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT tenant_id, auth_id, auth_index, provider, auth_subject_id,
			binding_seed_kind, binding_seed_hash, share_eligible, binding_state,
			binding_revision, bound_at, last_seen_at, deleted_at
		FROM ai_account_tenant_bindings
		WHERE tenant_id = ? AND auth_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list ai account bindings: %w", err)
	}
	defer rows.Close()
	out := make([]AIAccountTenantBinding, 0, len(ids))
	for rows.Next() {
		var row AIAccountTenantBinding
		var share bool
		var bound, seen, deleted sql.NullString
		if err := rows.Scan(&row.TenantID, &row.AuthID, &row.AuthIndex, &row.Provider, &row.AuthSubjectID,
			&row.BindingSeedKind, &row.BindingSeedHash, &share, &row.BindingState, &row.BindingRevision,
			&bound, &seen, &deleted); err != nil {
			return nil, err
		}
		row.ShareEligible = share
		if t, ok := parseStoredTimeString(bound.String); ok {
			row.BoundAt = t
		}
		if t, ok := parseStoredTimeString(seen.String); ok {
			row.LastSeenAt = t
		}
		row.DeletedAt = parseNullableTime(deleted)
		out = append(out, row)
	}
	return out, rows.Err()
}

func CountAIAccountTenantBindings(tenantID string, subjectIDs []string) (map[string]int, error) {
	db := getReadDB()
	out := make(map[string]int)
	ids := dedupeExactStrings(subjectIDs)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	args := []any{normalizeTenantID(tenantID)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, COUNT(*)
		FROM ai_account_tenant_bindings
		WHERE tenant_id = ? AND binding_state = 'active'
		  AND auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
		GROUP BY auth_subject_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		out[id] = count
	}
	return out, rows.Err()
}

func ListAIAccountSubjects(subjectIDs []string) (map[string]AIAccountSubjectRecord, error) {
	db := getReadDB()
	out := make(map[string]AIAccountSubjectRecord)
	ids := dedupeExactStrings(subjectIDs)
	if db == nil || len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, subject_scope, seed_kind, seed_hash,
			share_eligible, usage_projected_since, usage_history_complete, created_at, updated_at
		FROM ai_account_subjects
		WHERE auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row AIAccountSubjectRecord
		var projected, created, updated sql.NullString
		if err := rows.Scan(&row.AuthSubjectID, &row.Provider, &row.SubjectScope, &row.SeedKind, &row.SeedHash,
			&row.ShareEligible, &projected, &row.UsageHistoryComplete, &created, &updated); err != nil {
			return nil, err
		}
		row.UsageProjectedSince = parseNullableTime(projected)
		if t, ok := parseStoredTimeString(created.String); ok {
			row.CreatedAt = t
		}
		if t, ok := parseStoredTimeString(updated.String); ok {
			row.UpdatedAt = t
		}
		out[row.AuthSubjectID] = row
	}
	return out, rows.Err()
}

func UpsertAIAccountSubjectStatus(record AIAccountSubjectStatusRecord) error {
	db := getDB()
	if db == nil || strings.TrimSpace(record.AuthSubjectID) == "" {
		return nil
	}
	if record.LastProbeState == "" {
		record.LastProbeState = "idle"
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	if record.Version < 1 {
		record.Version = 1
	}
	quotaJSON, err := json.Marshal(record.Quotas)
	if err != nil {
		return err
	}
	expJSON, err := json.Marshal(record.ResetCreditExpirations)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO ai_account_subject_status (
			auth_subject_id, provider, last_probe_state, health_status, plan_type,
			subscription_started_at, subscription_expires_at, subscription_source,
			restriction_summary, error_code, error_summary, quota_json,
			reset_credit_count, reset_credit_expirations, upstream_checked_at, version, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			provider = excluded.provider,
			last_probe_state = excluded.last_probe_state,
			health_status = excluded.health_status,
			plan_type = excluded.plan_type,
			subscription_started_at = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_started_at ELSE ai_account_subject_status.subscription_started_at END,
			subscription_expires_at = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_expires_at ELSE ai_account_subject_status.subscription_expires_at END,
			subscription_source = CASE WHEN excluded.subscription_source <> ''
				THEN excluded.subscription_source ELSE ai_account_subject_status.subscription_source END,
			restriction_summary = excluded.restriction_summary,
			error_code = excluded.error_code,
			error_summary = excluded.error_summary,
			quota_json = excluded.quota_json,
			reset_credit_count = excluded.reset_credit_count,
			reset_credit_expirations = excluded.reset_credit_expirations,
			upstream_checked_at = excluded.upstream_checked_at,
			version = CASE WHEN excluded.version > ai_account_subject_status.version
				THEN excluded.version ELSE ai_account_subject_status.version + 1 END,
			updated_at = excluded.updated_at
	`, record.AuthSubjectID, record.Provider, record.LastProbeState, record.HealthStatus, record.PlanType,
		nullableTimeValue(record.SubscriptionStartedAt), nullableTimeValue(record.SubscriptionExpiresAt), record.SubscriptionSource,
		record.RestrictionSummary, record.ErrorCode, record.ErrorSummary, string(quotaJSON), record.ResetCreditCount,
		string(expJSON), nullableTimeValue(record.UpstreamCheckedAt), record.Version, record.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("usage: upsert shared ai account status: %w", err)
	}
	return nil
}

func UpdateAIAccountSubjectProbeFailure(subjectID, provider, errorCode, errorSummary string, checked time.Time) error {
	db := getDB()
	if db == nil || strings.TrimSpace(subjectID) == "" {
		return nil
	}
	if checked.IsZero() {
		checked = time.Now().UTC()
	}
	_, err := db.Exec(`
		INSERT INTO ai_account_subject_status (
			auth_subject_id, provider, last_probe_state, error_code, error_summary,
			upstream_checked_at, version, updated_at
		) VALUES (?, ?, 'error', ?, ?, ?, 1, ?)
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			provider = excluded.provider,
			last_probe_state = 'error',
			error_code = excluded.error_code,
			error_summary = excluded.error_summary,
			upstream_checked_at = excluded.upstream_checked_at,
			version = ai_account_subject_status.version + 1,
			updated_at = excluded.updated_at
	`, subjectID, provider, strings.TrimSpace(errorCode), sanitizeSharedStatusError(errorSummary),
		checked.UTC().Format(time.RFC3339Nano), checked.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("usage: update shared ai account failure: %w", err)
	}
	return nil
}

func sanitizeSharedStatusError(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "timeout"), strings.Contains(value, "deadline"):
		return "upstream status probe timed out"
	case strings.Contains(value, "unauthorized"), strings.Contains(value, "http 401"):
		return "upstream authorization failed"
	case strings.Contains(value, "forbidden"), strings.Contains(value, "http 403"):
		return "upstream access was denied"
	default:
		return "upstream status probe failed"
	}
}

func ListAIAccountSubjectStatus(subjectIDs []string) ([]AIAccountSubjectStatusRecord, error) {
	db := getReadDB()
	ids := dedupeExactStrings(subjectIDs)
	if db == nil || len(ids) == 0 {
		return []AIAccountSubjectStatusRecord{}, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.Query(`
		SELECT auth_subject_id, provider, last_probe_state, health_status, plan_type,
			subscription_started_at, subscription_expires_at, subscription_source,
			restriction_summary, error_code, error_summary, quota_json,
			reset_credit_count, reset_credit_expirations, upstream_checked_at, version, updated_at
		FROM ai_account_subject_status
		WHERE auth_subject_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list shared ai account status: %w", err)
	}
	defer rows.Close()
	out := make([]AIAccountSubjectStatusRecord, 0, len(ids))
	for rows.Next() {
		var row AIAccountSubjectStatusRecord
		var started, expires, checked, updated sql.NullString
		var reset sql.NullInt64
		var quotaJSON, expJSON string
		if err := rows.Scan(&row.AuthSubjectID, &row.Provider, &row.LastProbeState, &row.HealthStatus, &row.PlanType,
			&started, &expires, &row.SubscriptionSource, &row.RestrictionSummary, &row.ErrorCode, &row.ErrorSummary,
			&quotaJSON, &reset, &expJSON, &checked, &row.Version, &updated); err != nil {
			return nil, err
		}
		row.SubscriptionStartedAt = parseNullableTime(started)
		row.SubscriptionExpiresAt = parseNullableTime(expires)
		row.UpstreamCheckedAt = parseNullableTime(checked)
		row.Quotas = decodeQuotaWindows(quotaJSON)
		row.ResetCreditExpirations = decodeStringSlice(expJSON)
		if reset.Valid {
			v := reset.Int64
			row.ResetCreditCount = &v
		}
		if t, ok := parseStoredTimeString(updated.String); ok {
			row.UpdatedAt = t
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func nullableTimeValue(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// BackfillAIAccountSharedSubject merges only existing small projections. It
// never writes cycle usage because legacy daily rows lack exact cycle timestamps.
func BackfillAIAccountSharedSubject(subjectID string) error {
	db := getDB()
	subjectID = strings.TrimSpace(subjectID)
	if db == nil || subjectID == "" {
		return nil
	}
	if err := backfillSharedStatusForSubject(db, subjectID); err != nil {
		return err
	}
	if err := backfillSharedDailyUsageForSubject(db, subjectID); err != nil {
		return err
	}
	if err := backfillSharedLifetimeForSubject(db, subjectID); err != nil {
		return err
	}
	if err := backfillSharedQuotaCyclesForSubject(db, subjectID); err != nil {
		return err
	}
	return backfillSharedQuotaPointsForSubject(db, subjectID)
}

func backfillSharedStatusForSubject(db *sql.DB, subjectID string) error {
	var row AIAccountStatusRecord
	var quotaJSON, expJSON string
	var reset sql.NullInt64
	var upstream, usageUpdated, expires, updated sql.NullString
	err := db.QueryRow(`
		SELECT tenant_id, auth_subject_id, auth_index, provider, refresh_state, health_status, plan_type,
			restriction_summary, error_summary, error_code, error_message, quota_json,
			reset_credit_count, reset_credit_expirations, upstream_checked_at, usage_updated_at,
			expires_at, version, updated_at
		FROM ai_account_status WHERE auth_subject_id = ?
		ORDER BY COALESCE(upstream_checked_at, updated_at) DESC, updated_at DESC, version DESC LIMIT 1
	`, subjectID).Scan(&row.TenantID, &row.AuthSubjectID, &row.AuthIndex, &row.Provider, &row.RefreshState,
		&row.HealthStatus, &row.PlanType, &row.RestrictionSummary, &row.ErrorSummary, &row.ErrorCode,
		&row.ErrorMessage, &quotaJSON, &reset, &expJSON, &upstream, &usageUpdated, &expires, &row.Version, &updated)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("usage: backfill shared status: %w", err)
	}
	row.Quotas = decodeQuotaWindows(quotaJSON)
	row.ResetCreditExpirations = decodeStringSlice(expJSON)
	if reset.Valid {
		v := reset.Int64
		row.ResetCreditCount = &v
	}
	row.UpstreamCheckedAt = parseNullableTime(upstream)
	row.ExpiresAt = parseNullableTime(expires)
	if t, ok := parseStoredTimeString(updated.String); ok {
		row.UpdatedAt = t
	}
	var sharedUpdated sql.NullString
	if err := db.QueryRow(`SELECT updated_at FROM ai_account_subject_status WHERE auth_subject_id = ?`, subjectID).Scan(&sharedUpdated); err == nil {
		if t, ok := parseStoredTimeString(sharedUpdated.String); ok && !row.UpdatedAt.After(t) {
			return nil
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	errorSummary := ""
	if normalizeProbeState(row.RefreshState) == "error" {
		errorSummary = sanitizeSharedStatusError(row.ErrorSummary)
	}
	subscriptionSource := ""
	if row.ExpiresAt != nil {
		subscriptionSource = "migration"
	}
	return UpsertAIAccountSubjectStatus(AIAccountSubjectStatusRecord{
		AuthSubjectID: row.AuthSubjectID, Provider: row.Provider, LastProbeState: normalizeProbeState(row.RefreshState),
		HealthStatus: row.HealthStatus, PlanType: row.PlanType, SubscriptionExpiresAt: row.ExpiresAt,
		SubscriptionSource: subscriptionSource, RestrictionSummary: row.RestrictionSummary, ErrorCode: row.ErrorCode,
		ErrorSummary: errorSummary, Quotas: row.Quotas, ResetCreditCount: row.ResetCreditCount,
		ResetCreditExpirations: row.ResetCreditExpirations, UpstreamCheckedAt: row.UpstreamCheckedAt, UpdatedAt: row.UpdatedAt,
	})
}

func normalizeProbeState(value string) string {
	switch strings.TrimSpace(value) {
	case "success":
		return "success"
	case "error":
		return "error"
	default:
		return "idle"
	}
}

func backfillSharedDailyUsageForSubject(db *sql.DB, subjectID string) error {
	rows, err := db.Query(`
		SELECT day_key, SUM(request_count), SUM(success_count), SUM(failure_count), SUM(cost_total), MIN(updated_at), MAX(updated_at)
		FROM auth_subject_usage_daily WHERE auth_subject_id = ? GROUP BY day_key
	`, subjectID)
	if err != nil {
		return fmt.Errorf("usage: backfill shared daily usage: %w", err)
	}
	type dailyRow struct {
		day                   string
		req, success, failure int64
		cost                  float64
		first, updated        string
	}
	batch := make([]dailyRow, 0)
	for rows.Next() {
		var row dailyRow
		if err := rows.Scan(&row.day, &row.req, &row.success, &row.failure, &row.cost, &row.first, &row.updated); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range batch {
		firstValue := nullableStoredTimeArg(row.first)
		updatedValue := nullableStoredTimeArg(row.updated)
		if firstValue == nil {
			firstValue = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if updatedValue == nil {
			updatedValue = firstValue
		}
		if _, err := db.Exec(`
			INSERT INTO ai_account_subject_usage_buckets
				(auth_subject_id, bucket_kind, bucket_start, request_count, success_count, failure_count, cost_total, first_event_at, updated_at)
			VALUES (?, 'day', ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
				request_count = CASE WHEN excluded.request_count > ai_account_subject_usage_buckets.request_count THEN excluded.request_count ELSE ai_account_subject_usage_buckets.request_count END,
				success_count = CASE WHEN excluded.success_count > ai_account_subject_usage_buckets.success_count THEN excluded.success_count ELSE ai_account_subject_usage_buckets.success_count END,
				failure_count = CASE WHEN excluded.failure_count > ai_account_subject_usage_buckets.failure_count THEN excluded.failure_count ELSE ai_account_subject_usage_buckets.failure_count END,
				cost_total = CASE WHEN excluded.cost_total > ai_account_subject_usage_buckets.cost_total THEN excluded.cost_total ELSE ai_account_subject_usage_buckets.cost_total END,
				updated_at = CASE WHEN excluded.updated_at > ai_account_subject_usage_buckets.updated_at THEN excluded.updated_at ELSE ai_account_subject_usage_buckets.updated_at END
		`, subjectID, row.day, row.req, row.success, row.failure, row.cost, firstValue, updatedValue); err != nil {
			return err
		}
	}
	return nil
}

func backfillSharedLifetimeForSubject(db *sql.DB, subjectID string) error {
	var req, success, failure int64
	var cost float64
	var first, updated sql.NullString
	err := db.QueryRow(`
		SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(success_count),0),
			COALESCE(SUM(failure_count),0), COALESCE(SUM(cost_total),0),
			MIN(updated_at), MAX(updated_at)
		FROM usage_rollup_buckets
		WHERE bucket_kind = 'lifetime' AND auth_subject_id = ?
	`, subjectID).Scan(&req, &success, &failure, &cost, &first, &updated)
	if err != nil {
		return fmt.Errorf("usage: backfill shared lifetime: %w", err)
	}
	if req == 0 {
		return nil
	}
	firstValue := first.String
	if firstValue == "" {
		firstValue = time.Now().UTC().Format(time.RFC3339Nano)
	}
	updatedValue := updated.String
	if updatedValue == "" {
		updatedValue = firstValue
	}
	if _, err := db.Exec(`
		INSERT INTO ai_account_subject_usage_buckets
			(auth_subject_id, bucket_kind, bucket_start, request_count, success_count, failure_count, cost_total, first_event_at, updated_at)
		VALUES (?, 'lifetime', '1970-01-01', ?, ?, ?, ?, ?, ?)
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			request_count = CASE WHEN excluded.request_count > ai_account_subject_usage_buckets.request_count THEN excluded.request_count ELSE ai_account_subject_usage_buckets.request_count END,
			success_count = CASE WHEN excluded.success_count > ai_account_subject_usage_buckets.success_count THEN excluded.success_count ELSE ai_account_subject_usage_buckets.success_count END,
			failure_count = CASE WHEN excluded.failure_count > ai_account_subject_usage_buckets.failure_count THEN excluded.failure_count ELSE ai_account_subject_usage_buckets.failure_count END,
			cost_total = CASE WHEN excluded.cost_total > ai_account_subject_usage_buckets.cost_total THEN excluded.cost_total ELSE ai_account_subject_usage_buckets.cost_total END,
			updated_at = CASE WHEN excluded.updated_at > ai_account_subject_usage_buckets.updated_at THEN excluded.updated_at ELSE ai_account_subject_usage_buckets.updated_at END
	`, subjectID, req, success, failure, cost, firstValue, updatedValue); err != nil {
		return err
	}
	complete := projectionMarkerValue(db, usageRollupBackfillMarker) == rollupMarkerDone
	_, err = db.Exec(`
		UPDATE ai_account_subjects
		SET usage_projected_since = COALESCE(usage_projected_since, ?),
			usage_history_complete = ?, updated_at = ?
		WHERE auth_subject_id = ?
	`, firstValue, complete, time.Now().UTC().Format(time.RFC3339Nano), subjectID)
	return err
}

// backfillSharedQuotaCyclesForSubject migrates cycle definitions only. Cycle
// usage is projected from requests with exact timestamps.
func backfillSharedQuotaCyclesForSubject(db *sql.DB, subjectID string) error {
	rows, err := db.Query(`
		SELECT provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at
		FROM auth_subject_quota_cycles WHERE subject_id = ?
		ORDER BY last_verified_at DESC
	`, subjectID)
	if err != nil {
		return fmt.Errorf("usage: backfill shared quota cycles: %w", err)
	}
	type cycleRow struct {
		provider, key, start, reset, verified string
		window                                int64
	}
	batch := make([]cycleRow, 0)
	seen := map[string]struct{}{}
	for rows.Next() {
		var row cycleRow
		if err := rows.Scan(&row.provider, &row.key, &row.start, &row.reset, &row.window, &row.verified); err != nil {
			rows.Close()
			return err
		}
		if _, ok := seen[row.key]; ok {
			continue
		}
		seen[row.key] = struct{}{}
		batch = append(batch, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range batch {
		startValue, okStart := requiredStoredTimeArg(row.start)
		resetValue, okReset := requiredStoredTimeArg(row.reset)
		verifiedValue, okVerified := requiredStoredTimeArg(row.verified)
		// Postgres rejects empty strings for timestamptz; skip unusable legacy rows.
		if !okStart || !okReset || !okVerified {
			continue
		}
		if _, err := db.Exec(`
			INSERT INTO ai_account_subject_quota_cycles
				(auth_subject_id, provider, quota_key, cycle_start_at, reset_at, window_seconds, last_verified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(auth_subject_id, quota_key) DO UPDATE SET
				provider = CASE WHEN excluded.last_verified_at > ai_account_subject_quota_cycles.last_verified_at THEN excluded.provider ELSE ai_account_subject_quota_cycles.provider END,
				cycle_start_at = CASE WHEN excluded.last_verified_at > ai_account_subject_quota_cycles.last_verified_at THEN excluded.cycle_start_at ELSE ai_account_subject_quota_cycles.cycle_start_at END,
				reset_at = CASE WHEN excluded.last_verified_at > ai_account_subject_quota_cycles.last_verified_at THEN excluded.reset_at ELSE ai_account_subject_quota_cycles.reset_at END,
				window_seconds = CASE WHEN excluded.last_verified_at > ai_account_subject_quota_cycles.last_verified_at THEN excluded.window_seconds ELSE ai_account_subject_quota_cycles.window_seconds END,
				last_verified_at = CASE WHEN excluded.last_verified_at > ai_account_subject_quota_cycles.last_verified_at THEN excluded.last_verified_at ELSE ai_account_subject_quota_cycles.last_verified_at END
		`, subjectID, row.provider, row.key, startValue, resetValue, row.window, verifiedValue); err != nil {
			return err
		}
	}
	return nil
}

// nullableStoredTimeArg returns a canonical RFC3339Nano string or nil (SQL NULL).
// Never returns "" — Postgres timestamptz rejects empty strings (SQLSTATE 22007).
func nullableStoredTimeArg(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if parsed, ok := parseStoredTimeString(value); ok {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	return nil
}

func requiredStoredTimeArg(value string) (string, bool) {
	arg := nullableStoredTimeArg(value)
	if arg == nil {
		return "", false
	}
	s, ok := arg.(string)
	return s, ok && s != ""
}

func backfillSharedQuotaPointsForSubject(db *sql.DB, subjectID string) error {
	rows, err := db.Query(`
		SELECT provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at
		FROM auth_file_quota_snapshot_points
		WHERE auth_subject_id = ?
		ORDER BY recorded_at, quota_key
	`, subjectID)
	if err != nil {
		return fmt.Errorf("usage: backfill shared quota points: %w", err)
	}
	type pointRow struct {
		provider, key, label, recorded string
		percent                        sql.NullFloat64
		reset                          sql.NullString
		window                         int64
	}
	batch := make([]pointRow, 0)
	for rows.Next() {
		var row pointRow
		if err := rows.Scan(&row.provider, &row.key, &row.label, &row.percent, &row.reset, &row.window, &row.recorded); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range batch {
		var percentValue any
		if row.percent.Valid {
			percentValue = row.percent.Float64
		}
		// NULL, not "": PG timestamptz rejects "".
		var resetValue any
		if row.reset.Valid {
			resetValue = nullableStoredTimeArg(row.reset.String)
		}
		recordedValue, okRecorded := requiredStoredTimeArg(row.recorded)
		if !okRecorded {
			continue
		}
		// Skip EXISTS check when reset is NULL: untyped NULL params confuse PG (42P18).
		// Rare path; duplicate insert is prevented by natural uniqueness + ignore conflicts.
		if resetValue == nil {
			_, err := db.Exec(`
				INSERT INTO ai_account_subject_quota_points
					(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
				SELECT ?, ?, ?, ?, ?, NULL, ?, ?
				WHERE NOT EXISTS (
					SELECT 1 FROM ai_account_subject_quota_points
					WHERE auth_subject_id = ? AND provider = ? AND quota_key = ? AND quota_label = ?
					  AND COALESCE(percent, -1) = COALESCE(?, -1)
					  AND reset_at IS NULL
					  AND window_seconds = ? AND recorded_at = ?
				)
			`, subjectID, row.provider, row.key, row.label, percentValue, row.window, recordedValue,
				subjectID, row.provider, row.key, row.label, percentValue, row.window, recordedValue)
			if err != nil {
				return err
			}
			continue
		}
		if _, err := db.Exec(`
			INSERT INTO ai_account_subject_quota_points
				(auth_subject_id, provider, quota_key, quota_label, percent, reset_at, window_seconds, recorded_at)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (
				SELECT 1 FROM ai_account_subject_quota_points
				WHERE auth_subject_id = ? AND provider = ? AND quota_key = ? AND quota_label = ?
				  AND COALESCE(percent, -1) = COALESCE(?, -1)
				  AND reset_at = ?
				  AND window_seconds = ? AND recorded_at = ?
			)
		`, subjectID, row.provider, row.key, row.label, percentValue, resetValue, row.window, recordedValue,
			subjectID, row.provider, row.key, row.label, percentValue, resetValue, row.window, recordedValue); err != nil {
			return err
		}
	}
	return nil
}

func RunAIAccountSharedSubjectBackfillAtInit() (AIAccountSharedBackfillReport, error) {
	db := getDB()
	if db == nil {
		return AIAccountSharedBackfillReport{}, nil
	}
	if projectionMarkerValue(db, aiAccountSharedBackfillMarker) == "done" {
		return collectAIAccountSharedBackfillReport(db)
	}
	if err := setProjectionMarker(db, aiAccountSharedBackfillMarker, "pending"); err != nil {
		return AIAccountSharedBackfillReport{}, err
	}
	rows, err := db.Query(`SELECT auth_subject_id FROM ai_account_subjects ORDER BY auth_subject_id`)
	if err != nil {
		return AIAccountSharedBackfillReport{}, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return AIAccountSharedBackfillReport{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return AIAccountSharedBackfillReport{}, err
	}
	for _, id := range ids {
		if err := BackfillAIAccountSharedSubject(id); err != nil {
			return AIAccountSharedBackfillReport{}, err
		}
	}
	report, err := collectAIAccountSharedBackfillReport(db)
	if err != nil {
		return AIAccountSharedBackfillReport{}, err
	}
	if err := setProjectionMarker(db, aiAccountSharedBackfillMarker, "done"); err != nil {
		return AIAccountSharedBackfillReport{}, err
	}
	return report, nil
}

func collectAIAccountSharedBackfillReport(db *sql.DB) (AIAccountSharedBackfillReport, error) {
	var report AIAccountSharedBackfillReport
	for _, item := range []struct {
		query  string
		target *int64
	}{
		{`SELECT COUNT(*) FROM ai_account_subjects`, &report.Subjects},
		{`SELECT COUNT(*) FROM ai_account_tenant_bindings WHERE binding_state='active'`, &report.Bindings},
		{`SELECT COUNT(*) FROM ai_account_subject_status`, &report.StatusRows},
		{`SELECT COUNT(*) FROM ai_account_subject_usage_buckets`, &report.UsageRows},
		{`SELECT COUNT(*) FROM ai_account_subject_quota_cycles`, &report.QuotaCycles},
		{`SELECT COUNT(*) FROM ai_account_subject_quota_points`, &report.QuotaPoints},
	} {
		if err := db.QueryRow(item.query).Scan(item.target); err != nil {
			return report, err
		}
	}
	if err := db.QueryRow(`SELECT COUNT(DISTINCT auth_subject_id) FROM ai_account_status WHERE auth_subject_id NOT IN (SELECT auth_subject_id FROM ai_account_subjects)`).Scan(&report.UnboundLegacySubjects); err != nil {
		return report, err
	}
	conflicts, err := countAIAccountLegacyStatusPayloadConflicts(db)
	if err != nil {
		return report, err
	}
	report.StatusPayloadConflicts = conflicts
	checksumParts := []string{
		fmt.Sprint(report.Subjects), fmt.Sprint(report.Bindings), fmt.Sprint(report.StatusRows),
		fmt.Sprint(report.UsageRows), fmt.Sprint(report.QuotaCycles), fmt.Sprint(report.QuotaPoints),
		fmt.Sprint(report.UnboundLegacySubjects), fmt.Sprint(report.StatusPayloadConflicts),
	}
	report.Checksum = stableSeedHash(strings.Join(checksumParts, "|"))
	return report, nil
}

func countAIAccountLegacyStatusPayloadConflicts(db *sql.DB) (int64, error) {
	rows, err := db.Query(`
		SELECT auth_subject_id, plan_type, restriction_summary, quota_json,
			reset_credit_count, reset_credit_expirations, expires_at
		FROM ai_account_status
		WHERE trim(auth_subject_id) <> ''
		ORDER BY auth_subject_id
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	seen := make(map[string]map[string]struct{})
	for rows.Next() {
		var subjectID, planType, restriction, quotaJSON, expirations string
		var reset sql.NullInt64
		var expires sql.NullString
		if err := rows.Scan(&subjectID, &planType, &restriction, &quotaJSON, &reset, &expirations, &expires); err != nil {
			return 0, err
		}
		resetValue := ""
		if reset.Valid {
			resetValue = fmt.Sprint(reset.Int64)
		}
		payloadHash := stableSeedHash(strings.Join([]string{
			planType, restriction, quotaJSON, resetValue, expirations, expires.String,
		}, "\x1f"))
		if seen[subjectID] == nil {
			seen[subjectID] = make(map[string]struct{})
		}
		seen[subjectID][payloadHash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var conflicts int64
	for _, hashes := range seen {
		if len(hashes) > 1 {
			conflicts++
		}
	}
	return conflicts, nil
}

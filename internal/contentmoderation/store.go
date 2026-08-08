package contentmoderation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const createTablesSQL = `
CREATE TABLE IF NOT EXISTS content_moderation_profiles (
  tenant_id TEXT NOT NULL,
  id TEXT NOT NULL,
  name TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT 'off',
  backend TEXT NOT NULL DEFAULT 'openai_moderations',
  base_url TEXT NOT NULL DEFAULT 'https://api.openai.com',
  model TEXT NOT NULL DEFAULT 'omni-moderation-latest',
  api_key_secret TEXT NOT NULL DEFAULT '',
  timeout_ms INTEGER NOT NULL DEFAULT 3000,
  keyword_mode TEXT NOT NULL DEFAULT 'api_only',
  blocked_keywords_json TEXT NOT NULL DEFAULT '[]',
  thresholds_json TEXT NOT NULL DEFAULT '{}',
  scanners_json TEXT NOT NULL DEFAULT '[]',
  controversial_action TEXT NOT NULL DEFAULT 'elevated_only',
  elevated_categories_json TEXT NOT NULL DEFAULT '["jailbreak","pii","suicide_and_self_harm"]',
  input_limit INTEGER NOT NULL DEFAULT 4000,
  max_chunks INTEGER NOT NULL DEFAULT 4,
  block_http_status INTEGER NOT NULL DEFAULT 403,
  block_message TEXT NOT NULL DEFAULT '',
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS content_moderation_channel_bindings (
  tenant_id TEXT NOT NULL,
  channel_type TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, channel_type, channel_id),
  FOREIGN KEY (tenant_id, profile_id)
    REFERENCES content_moderation_profiles(tenant_id, id)
    ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_content_moderation_bindings_profile
  ON content_moderation_channel_bindings(tenant_id, profile_id);
`

// profileColumnMigrations backfill the moderation-backend columns onto tables
// created before qwen3guard support existed. The defaults reproduce the
// previous behaviour exactly, so existing profiles keep running against the
// OpenAI moderations API with no configuration change.
var profileColumnMigrations = []string{
	`ALTER TABLE content_moderation_profiles ADD COLUMN backend TEXT NOT NULL DEFAULT 'openai_moderations'`,
	`ALTER TABLE content_moderation_profiles ADD COLUMN scanners_json TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE content_moderation_profiles ADD COLUMN controversial_action TEXT NOT NULL DEFAULT 'elevated_only'`,
	`ALTER TABLE content_moderation_profiles ADD COLUMN elevated_categories_json TEXT NOT NULL DEFAULT '["jailbreak","pii","suicide_and_self_harm"]'`,
	`ALTER TABLE content_moderation_profiles ADD COLUMN input_limit INTEGER NOT NULL DEFAULT 4000`,
	`ALTER TABLE content_moderation_profiles ADD COLUMN max_chunks INTEGER NOT NULL DEFAULT 4`,
}

func InitTables(db *sql.DB) error {
	if db == nil {
		return ErrUnavailable
	}
	if _, err := db.Exec(createTablesSQL); err != nil {
		return fmt.Errorf("create content moderation tables: %w", err)
	}
	for _, stmt := range profileColumnMigrations {
		_, _ = db.Exec(stmt) // ignore duplicate-column errors on already-migrated installs
	}
	return nil
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) ListProfiles(ctx context.Context, tenantID string) ([]Profile, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, profileSelectSQL+` WHERE tenant_id = $1 ORDER BY lower(name), id`, strings.TrimSpace(tenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]Profile, 0)
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (s *Store) GetProfile(ctx context.Context, tenantID, id string) (Profile, error) {
	if s == nil || s.db == nil {
		return Profile{}, ErrUnavailable
	}
	profile, err := scanProfile(s.db.QueryRowContext(ctx, profileSelectSQL+` WHERE tenant_id = $1 AND id = $2`, strings.TrimSpace(tenantID), strings.TrimSpace(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	return profile, err
}

func (s *Store) CreateProfile(ctx context.Context, profile Profile) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	columns, err := profileJSON(profile)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO content_moderation_profiles
		(tenant_id,id,name,mode,backend,base_url,model,api_key_secret,timeout_ms,keyword_mode,blocked_keywords_json,thresholds_json,
		 scanners_json,controversial_action,elevated_categories_json,input_limit,max_chunks,block_http_status,block_message,version,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		profile.TenantID, profile.ID, profile.Name, profile.Mode, profile.Backend, profile.BaseURL, profile.Model, profile.APIKeySecret,
		profile.TimeoutMS, profile.KeywordMode, columns.Keywords, columns.Thresholds,
		columns.Scanners, profile.ControversialAction, columns.Elevated, profile.InputLimit, profile.MaxChunks,
		profile.BlockHTTPStatus, profile.BlockMessage,
		profile.Version, formatTime(profile.CreatedAt), formatTime(profile.UpdatedAt))
	if isUniqueViolation(err) {
		return ErrNameConflict
	}
	return err
}

func (s *Store) UpdateProfile(ctx context.Context, profile Profile, expectedVersion int64) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	columns, err := profileJSON(profile)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE content_moderation_profiles SET
		name=$1,mode=$2,backend=$3,base_url=$4,model=$5,api_key_secret=$6,timeout_ms=$7,keyword_mode=$8,
		blocked_keywords_json=$9,thresholds_json=$10,scanners_json=$11,controversial_action=$12,elevated_categories_json=$13,
		input_limit=$14,max_chunks=$15,block_http_status=$16,block_message=$17,version=$18,updated_at=$19
		WHERE tenant_id=$20 AND id=$21 AND version=$22`,
		profile.Name, profile.Mode, profile.Backend, profile.BaseURL, profile.Model, profile.APIKeySecret, profile.TimeoutMS, profile.KeywordMode,
		columns.Keywords, columns.Thresholds, columns.Scanners, profile.ControversialAction, columns.Elevated,
		profile.InputLimit, profile.MaxChunks, profile.BlockHTTPStatus, profile.BlockMessage, profile.Version, formatTime(profile.UpdatedAt),
		profile.TenantID, profile.ID, expectedVersion)
	if isUniqueViolation(err) {
		return ErrNameConflict
	}
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		if _, getErr := s.GetProfile(ctx, profile.TenantID, profile.ID); errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrVersionConflict
	}
	return nil
}

func (s *Store) DeleteProfile(ctx context.Context, tenantID, id string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_moderation_channel_bindings WHERE tenant_id=$1 AND profile_id=$2`, tenantID, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return &ProfileBoundError{Count: count}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM content_moderation_profiles WHERE tenant_id=$1 AND id=$2`, tenantID, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) BindingCounts(ctx context.Context, tenantID string) (map[string]map[string]int, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT profile_id, channel_type, COUNT(*) FROM content_moderation_channel_bindings WHERE tenant_id=$1 GROUP BY profile_id, channel_type`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]int)
	for rows.Next() {
		var profileID, channelType string
		var count int
		if err := rows.Scan(&profileID, &channelType, &count); err != nil {
			return nil, err
		}
		if out[profileID] == nil {
			out[profileID] = make(map[string]int)
		}
		out[profileID][channelType] = count
	}
	return out, rows.Err()
}

func (s *Store) ListBindings(ctx context.Context, tenantID string) ([]Binding, error) {
	if s == nil || s.db == nil {
		return nil, ErrUnavailable
	}
	rows, err := s.db.QueryContext(ctx, `SELECT tenant_id,channel_type,channel_id,profile_id,created_at,updated_at FROM content_moderation_channel_bindings WHERE tenant_id=$1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]Binding, 0)
	for rows.Next() {
		var binding Binding
		var createdAt, updatedAt string
		if err := rows.Scan(&binding.TenantID, &binding.ChannelType, &binding.ChannelID, &binding.ProfileID, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		binding.CreatedAt = parseTime(createdAt)
		binding.UpdatedAt = parseTime(updatedAt)
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func (s *Store) PatchBindings(ctx context.Context, tenantID string, allowRebind bool, operations []BindingOperation) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatTime(time.Now().UTC())
	for _, operation := range operations {
		channelType := strings.TrimSpace(operation.ChannelType)
		channelID := strings.TrimSpace(operation.ChannelID)
		if !IsChannelType(channelType) || channelID == "" {
			return ErrInvalidChannel
		}
		if operation.ProfileID == nil || strings.TrimSpace(*operation.ProfileID) == "" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM content_moderation_channel_bindings WHERE tenant_id=$1 AND channel_type=$2 AND channel_id=$3`, tenantID, channelType, channelID); err != nil {
				return err
			}
			continue
		}
		profileID := strings.TrimSpace(*operation.ProfileID)
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM content_moderation_profiles WHERE tenant_id=$1 AND id=$2`, tenantID, profileID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT profile_id FROM content_moderation_channel_bindings WHERE tenant_id=$1 AND channel_type=$2 AND channel_id=$3`, tenantID, channelType, channelID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && existing != profileID && !allowRebind {
			return &BindingConflictError{ChannelType: channelType, ChannelID: channelID, ExistingProfileID: existing}
		}
		if err == nil && existing == profileID {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO content_moderation_channel_bindings
			(tenant_id,channel_type,channel_id,profile_id,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$5)
			ON CONFLICT(tenant_id,channel_type,channel_id) DO UPDATE SET profile_id=excluded.profile_id,updated_at=excluded.updated_at`,
			tenantID, channelType, channelID, profileID, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteChannelBinding(ctx context.Context, tenantID, channelType, channelID string) error {
	if s == nil || s.db == nil {
		return ErrUnavailable
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM content_moderation_channel_bindings WHERE tenant_id=$1 AND channel_type=$2 AND channel_id=$3`, tenantID, channelType, channelID)
	return err
}

func (s *Store) ResolveProfile(ctx context.Context, tenantID, authFileID, providerKeyID, providerID string) (Profile, string, error) {
	if s == nil || s.db == nil {
		return Profile{}, "", ErrUnavailable
	}
	lookups := []struct {
		channelType string
		channelID   string
	}{
		{ChannelTypeAuthFile, strings.TrimSpace(authFileID)},
		{ChannelTypeProviderKey, strings.TrimSpace(providerKeyID)},
		{ChannelTypeProvider, strings.TrimSpace(providerID)},
	}
	for _, lookup := range lookups {
		if lookup.channelID == "" {
			continue
		}
		profile, err := scanProfile(s.db.QueryRowContext(ctx, profileSelectSQL+`
			JOIN content_moderation_channel_bindings b ON b.tenant_id=content_moderation_profiles.tenant_id AND b.profile_id=content_moderation_profiles.id
			WHERE b.tenant_id=$1 AND b.channel_type=$2 AND b.channel_id=$3`, tenantID, lookup.channelType, lookup.channelID))
		if err == nil {
			return profile, lookup.channelType, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Profile{}, "", err
		}
	}
	return Profile{}, "", ErrNotFound
}

const profileSelectSQL = `SELECT content_moderation_profiles.tenant_id,content_moderation_profiles.id,content_moderation_profiles.name,content_moderation_profiles.mode,content_moderation_profiles.backend,content_moderation_profiles.base_url,content_moderation_profiles.model,content_moderation_profiles.api_key_secret,content_moderation_profiles.timeout_ms,content_moderation_profiles.keyword_mode,content_moderation_profiles.blocked_keywords_json,content_moderation_profiles.thresholds_json,content_moderation_profiles.scanners_json,content_moderation_profiles.controversial_action,content_moderation_profiles.elevated_categories_json,content_moderation_profiles.input_limit,content_moderation_profiles.max_chunks,content_moderation_profiles.block_http_status,content_moderation_profiles.block_message,content_moderation_profiles.version,content_moderation_profiles.created_at,content_moderation_profiles.updated_at FROM content_moderation_profiles`

func scanProfile(scanner interface{ Scan(...any) error }) (Profile, error) {
	var profile Profile
	var keywordsJSON, thresholdsJSON, scannersJSON, elevatedJSON, createdAt, updatedAt string
	if err := scanner.Scan(
		&profile.TenantID, &profile.ID, &profile.Name, &profile.Mode, &profile.Backend, &profile.BaseURL, &profile.Model, &profile.APIKeySecret,
		&profile.TimeoutMS, &profile.KeywordMode, &keywordsJSON, &thresholdsJSON, &scannersJSON, &profile.ControversialAction, &elevatedJSON,
		&profile.InputLimit, &profile.MaxChunks, &profile.BlockHTTPStatus, &profile.BlockMessage,
		&profile.Version, &createdAt, &updatedAt,
	); err != nil {
		return Profile{}, err
	}
	if err := json.Unmarshal([]byte(keywordsJSON), &profile.BlockedKeywords); err != nil {
		return Profile{}, fmt.Errorf("decode blocked keywords: %w", err)
	}
	if err := json.Unmarshal([]byte(thresholdsJSON), &profile.Thresholds); err != nil {
		return Profile{}, fmt.Errorf("decode thresholds: %w", err)
	}
	if err := json.Unmarshal([]byte(scannersJSON), &profile.Scanners); err != nil {
		return Profile{}, fmt.Errorf("decode scanners: %w", err)
	}
	if err := json.Unmarshal([]byte(elevatedJSON), &profile.ElevatedCategories); err != nil {
		return Profile{}, fmt.Errorf("decode elevated categories: %w", err)
	}
	profile.CreatedAt = parseTime(createdAt)
	profile.UpdatedAt = parseTime(updatedAt)
	return profile, nil
}

// profileJSONColumns holds the profile's JSON-encoded columns.
type profileJSONColumns struct {
	Keywords   string
	Thresholds string
	Scanners   string
	Elevated   string
}

func profileJSON(profile Profile) (profileJSONColumns, error) {
	keywords, err := json.Marshal(profile.BlockedKeywords)
	if err != nil {
		return profileJSONColumns{}, err
	}
	thresholds, err := json.Marshal(profile.Thresholds)
	if err != nil {
		return profileJSONColumns{}, err
	}
	scanners, err := json.Marshal(profile.Scanners)
	if err != nil {
		return profileJSONColumns{}, err
	}
	elevated, err := json.Marshal(profile.ElevatedCategories)
	if err != nil {
		return profileJSONColumns{}, err
	}
	return profileJSONColumns{
		Keywords:   string(keywords),
		Thresholds: string(thresholds),
		Scanners:   string(scanners),
		Elevated:   string(elevated),
	}, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed.UTC()
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate key")
}

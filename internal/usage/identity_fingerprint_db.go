package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	log "github.com/sirupsen/logrus"
)

const identityFingerprintLastSeenMinInterval = 5 * time.Minute

const createIdentityFingerprintsTableSQL = `
CREATE TABLE IF NOT EXISTS identity_fingerprints (
  provider          TEXT NOT NULL,
  account_key       TEXT NOT NULL,
  profile_key       TEXT NOT NULL DEFAULT 'default',
  auth_subject_id   TEXT NOT NULL DEFAULT '',
  client_product    TEXT NOT NULL DEFAULT '',
  client_variant    TEXT NOT NULL DEFAULT '',
  version           TEXT NOT NULL DEFAULT '',
  fields_json       TEXT NOT NULL DEFAULT '{}',
  observed_headers_json TEXT NOT NULL DEFAULT '{}',
  created_at        TEXT NOT NULL DEFAULT '',
  updated_at        TEXT NOT NULL DEFAULT '',
  last_seen_at      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, account_key, profile_key)
);

CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_provider_seen
  ON identity_fingerprints(provider, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_account_seen
  ON identity_fingerprints(provider, account_key, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS identity_fingerprint_account_policies (
  provider           TEXT NOT NULL,
  account_key        TEXT NOT NULL,
  strategy           TEXT NOT NULL DEFAULT 'cli_preferred',
  active_profile_key TEXT NOT NULL DEFAULT '',
  revision           INTEGER NOT NULL DEFAULT 1,
  updated_at         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (provider, account_key)
);
`

func initIdentityFingerprintsTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createIdentityFingerprintsTableSQL); err != nil {
		log.Errorf("usage: create identity fingerprint tables: %v", err)
	}
}

func ObserveIdentityFingerprint(input identityfingerprint.LearnInput) (*identityfingerprint.LearnedRecord, identityfingerprint.MergeResult, error) {
	if !ConfigStoreAvailable() {
		return nil, identityfingerprint.MergeResult{Reason: "store_unavailable"}, nil
	}
	input.AccountKey = strings.TrimSpace(input.AccountKey)
	if input.AccountKey == "" {
		return nil, identityfingerprint.MergeResult{Reason: "missing_account_key"}, nil
	}
	obs, ok := identityfingerprint.ExtractObservation(input)
	if !ok {
		return nil, identityfingerprint.MergeResult{Reason: "no_observation"}, nil
	}
	obs.ProfileKey = strings.TrimSpace(obs.ProfileKey)
	if obs.ProfileKey == "" {
		obs.ProfileKey = identityfingerprint.DefaultProfileKey(obs.Provider)
	}

	db := getDB()
	if db == nil {
		return nil, identityfingerprint.MergeResult{Reason: "store_unavailable"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, identityfingerprint.MergeResult{Reason: "begin_failed"}, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getIdentityFingerprintProfileWith(tx, obs.Provider, obs.AccountKey, obs.ProfileKey, currentUsageDriver() == "postgres")
	if err != nil {
		return nil, identityfingerprint.MergeResult{Reason: "load_failed"}, err
	}
	result := identityfingerprint.MergeObservation(existing, obs)
	if result.Record == nil || !result.Changed {
		if err := tx.Commit(); err != nil {
			return existing, result, err
		}
		return result.Record, result, nil
	}
	if existing != nil && !identityfingerprint.MergeObservationChangedExceptLastSeen(existing, result.Record) &&
		result.Record.LastSeenAt.Sub(existing.LastSeenAt) < identityFingerprintLastSeenMinInterval {
		if err := tx.Commit(); err != nil {
			return existing, result, err
		}
		return existing, identityfingerprint.MergeResult{
			Record:  existing,
			Changed: false,
			Reason:  "unchanged_recently_seen",
		}, nil
	}
	if err := upsertIdentityFingerprintWith(tx, result.Record); err != nil {
		return existing, result, err
	}
	if err := tx.Commit(); err != nil {
		return existing, result, err
	}
	notifyIdentityFingerprintInvalidated(result.Record.Provider, result.Record.AccountKey)
	return result.Record, result, nil
}

func GetIdentityFingerprint(provider identityfingerprint.Provider, accountKey string) (*identityfingerprint.LearnedRecord, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	if provider == "" || accountKey == "" {
		return nil, nil
	}
	row := db.QueryRow(`
		SELECT provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
		       fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		  FROM identity_fingerprints
		 WHERE provider = ? AND account_key = ?
		 ORDER BY last_seen_at DESC, profile_key ASC
		 LIMIT 1
	`, string(provider), accountKey)
	record, err := scanIdentityFingerprint(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

func GetIdentityFingerprintProfile(provider identityfingerprint.Provider, accountKey, profileKey string) (*identityfingerprint.LearnedRecord, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	return getIdentityFingerprintProfileWith(db, provider, accountKey, profileKey, false)
}

func getIdentityFingerprintProfileWith(queryer fingerprintQueryer, provider identityfingerprint.Provider, accountKey, profileKey string, forUpdate bool) (*identityfingerprint.LearnedRecord, error) {
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	profileKey = strings.TrimSpace(profileKey)
	if provider == "" || accountKey == "" || profileKey == "" {
		return nil, nil
	}
	query := `
		SELECT provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
		       fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		  FROM identity_fingerprints
		 WHERE provider = ? AND account_key = ? AND profile_key = ?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	record, err := scanIdentityFingerprint(queryer.QueryRow(query, string(provider), accountKey, profileKey))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}

func ListIdentityFingerprintProfiles(provider identityfingerprint.Provider, accountKey string) ([]identityfingerprint.LearnedRecord, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	if provider == "" || accountKey == "" {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
		       fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		  FROM identity_fingerprints
		 WHERE provider = ? AND account_key = ?
		 ORDER BY last_seen_at DESC, profile_key ASC
	`, string(provider), accountKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIdentityFingerprintRows(rows)
}

func ListIdentityFingerprints(provider identityfingerprint.Provider, limit int) ([]identityfingerprint.LearnedRecord, error) {
	db := getDB()
	if db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	providerText := strings.TrimSpace(string(provider))
	query := `
		SELECT provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
		       fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		  FROM identity_fingerprints
	`
	args := []any{}
	if providerText != "" {
		query += ` WHERE provider = ?`
		args = append(args, providerText)
	}
	query += ` ORDER BY last_seen_at DESC, profile_key ASC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIdentityFingerprintRows(rows)
}

func scanIdentityFingerprintRows(rows *sql.Rows) ([]identityfingerprint.LearnedRecord, error) {
	var records []identityfingerprint.LearnedRecord
	for rows.Next() {
		record, err := scanIdentityFingerprint(rows)
		if err != nil {
			return nil, err
		}
		if record != nil {
			records = append(records, *record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func UpsertIdentityFingerprint(record *identityfingerprint.LearnedRecord) error {
	db := getDB()
	if db == nil || record == nil {
		return nil
	}
	if err := upsertIdentityFingerprintWith(db, record); err != nil {
		return err
	}
	notifyIdentityFingerprintInvalidated(record.Provider, record.AccountKey)
	return nil
}

func upsertIdentityFingerprintWith(execer fingerprintExecer, record *identityfingerprint.LearnedRecord) error {
	if record == nil {
		return nil
	}
	normalizeIdentityFingerprintRecord(record)
	provider := strings.TrimSpace(string(record.Provider))
	accountKey := strings.TrimSpace(record.AccountKey)
	profileKey := strings.TrimSpace(record.ProfileKey)
	if provider == "" || accountKey == "" || profileKey == "" {
		return nil
	}
	fields, err := json.Marshal(nonNilStringMap(record.Fields))
	if err != nil {
		return err
	}
	observedHeaders, err := json.Marshal(nonNilStringMap(record.ObservedHeaders))
	if err != nil {
		return err
	}
	createdAt := formatFingerprintTime(record.CreatedAt)
	updatedAt := formatFingerprintTime(record.UpdatedAt)
	lastSeenAt := formatFingerprintTime(record.LastSeenAt)
	_, err = execer.Exec(`
		INSERT INTO identity_fingerprints (
			provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
			fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, account_key, profile_key) DO UPDATE SET
			auth_subject_id = excluded.auth_subject_id,
			client_product = excluded.client_product,
			client_variant = excluded.client_variant,
			version = excluded.version,
			fields_json = excluded.fields_json,
			observed_headers_json = excluded.observed_headers_json,
			updated_at = excluded.updated_at,
			last_seen_at = excluded.last_seen_at
	`, provider, accountKey, profileKey, strings.TrimSpace(record.AuthSubjectID), strings.TrimSpace(record.ClientProduct),
		strings.TrimSpace(record.ClientVariant), strings.TrimSpace(record.Version), string(fields), string(observedHeaders),
		createdAt, updatedAt, lastSeenAt)
	return err
}

func DeleteIdentityFingerprint(provider identityfingerprint.Provider, accountKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	if provider == "" || accountKey == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM identity_fingerprints WHERE provider = ? AND account_key = ?`, string(provider), accountKey)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err == nil && deleted > 0 {
		notifyIdentityFingerprintInvalidated(provider, accountKey)
	}
	return deleted, err
}

func DeleteIdentityFingerprintProfile(provider identityfingerprint.Provider, accountKey, profileKey string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	provider = identityfingerprint.Provider(strings.TrimSpace(string(provider)))
	accountKey = strings.TrimSpace(accountKey)
	profileKey = strings.TrimSpace(profileKey)
	if provider == "" || accountKey == "" || profileKey == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM identity_fingerprints WHERE provider = ? AND account_key = ? AND profile_key = ?`, string(provider), accountKey, profileKey)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err == nil && deleted > 0 {
		notifyIdentityFingerprintInvalidated(provider, accountKey)
	}
	return deleted, err
}

type fingerprintScanner interface {
	Scan(dest ...any) error
}

type fingerprintQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

type fingerprintExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func scanIdentityFingerprint(scanner fingerprintScanner) (*identityfingerprint.LearnedRecord, error) {
	var record identityfingerprint.LearnedRecord
	var provider string
	var fieldsJSON, observedJSON string
	var createdAt, updatedAt, lastSeenAt storedTime
	if err := scanner.Scan(
		&provider,
		&record.AccountKey,
		&record.ProfileKey,
		&record.AuthSubjectID,
		&record.ClientProduct,
		&record.ClientVariant,
		&record.Version,
		&fieldsJSON,
		&observedJSON,
		&createdAt,
		&updatedAt,
		&lastSeenAt,
	); err != nil {
		return nil, err
	}
	record.Provider = identityfingerprint.Provider(provider)
	normalizeIdentityFingerprintRecord(&record)
	record.Fields = map[string]string{}
	if strings.TrimSpace(fieldsJSON) != "" {
		if err := json.Unmarshal([]byte(fieldsJSON), &record.Fields); err != nil {
			return nil, fmt.Errorf("decode identity fingerprint fields: %w", err)
		}
	}
	record.ObservedHeaders = map[string]string{}
	if strings.TrimSpace(observedJSON) != "" {
		if err := json.Unmarshal([]byte(observedJSON), &record.ObservedHeaders); err != nil {
			return nil, fmt.Errorf("decode identity fingerprint observed headers: %w", err)
		}
	}
	if createdAt.Valid {
		record.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		record.UpdatedAt = updatedAt.Time
	}
	if lastSeenAt.Valid {
		record.LastSeenAt = lastSeenAt.Time
	}
	return &record, nil
}

func normalizeIdentityFingerprintRecord(record *identityfingerprint.LearnedRecord) {
	if record == nil {
		return
	}
	record.AccountKey = strings.TrimSpace(record.AccountKey)
	record.ProfileKey = strings.TrimSpace(record.ProfileKey)
	if record.Provider == identityfingerprint.ProviderCodex && record.ProfileKey == "" {
		if profileKey, _, ok := identityfingerprint.CodexProfileKey(
			record.Fields[identityfingerprint.FieldUserAgent],
			record.Fields[identityfingerprint.FieldCodexOriginator],
		); ok {
			record.ProfileKey = profileKey
		}
	}
	if record.ProfileKey == "" {
		record.ProfileKey = identityfingerprint.DefaultProfileKey(record.Provider)
	}
	if record.Provider == identityfingerprint.ProviderCodex {
		record.ProfileFamily = identityfingerprint.CodexProfileFamily(record.ProfileKey)
	}
}

func formatFingerprintTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nonNilStringMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

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
  tenant_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
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
  PRIMARY KEY (tenant_id, provider, account_key, profile_key)
);

CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_provider_seen
  ON identity_fingerprints(provider, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_identity_fingerprints_account_seen
  ON identity_fingerprints(provider, account_key, last_seen_at DESC);

CREATE TABLE IF NOT EXISTS identity_fingerprint_account_policies (
  tenant_id          TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  provider           TEXT NOT NULL,
  account_key        TEXT NOT NULL,
  strategy           TEXT NOT NULL DEFAULT 'cli_preferred',
  active_profile_key TEXT NOT NULL DEFAULT '',
  revision           INTEGER NOT NULL DEFAULT 1,
  updated_at         TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, provider, account_key)
);
`

func initIdentityFingerprintsTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createIdentityFingerprintsTableSQL); err != nil {
		log.Errorf("usage: create identity fingerprint tables: %v", err)
		return
	}
	if usageDriver != "postgres" {
		migrateIdentityFingerprintTenantSchema(db)
		if _, err := db.Exec(createIdentityFingerprintsTableSQL); err != nil {
			log.Errorf("usage: recreate identity fingerprint indexes: %v", err)
		}
	}
	purgeForeignAntigravityFingerprints(db)
}

type identityFingerprintKey struct {
	accountKey string
	profileKey string
}

// purgeForeignAntigravityFingerprints drops learned Antigravity identities whose
// User-Agent does not identify the Antigravity client.
//
// Until the observation guard was tightened, any non-empty User-Agent was
// learned and then replayed as that account's outbound identity — a Node client
// sends a bare "node" — and Antigravity refuses every request from a client it
// does not recognise with "You do not have a valid license of this product
// (#3501)". Dropping the row falls the account back to the built-in default
// identity, and a genuine client re-learns its own on the next request.
func purgeForeignAntigravityFingerprints(db *sql.DB) {
	if db == nil {
		return
	}
	stale, err := staleAntigravityFingerprints(db)
	if err != nil {
		log.Errorf("usage: scan learned antigravity identities: %v", err)
		return
	}
	provider := string(identityfingerprint.ProviderAntigravity)
	for _, key := range stale {
		if _, err = db.Exec(`DELETE FROM identity_fingerprints WHERE tenant_id = ? AND provider = ? AND account_key = ? AND profile_key = ?`,
			systemTenantID, provider, key.accountKey, key.profileKey); err != nil {
			log.Errorf("usage: purge learned antigravity identity %s/%s: %v", key.accountKey, key.profileKey, err)
			continue
		}
		notifyIdentityFingerprintInvalidated(identityfingerprint.ProviderAntigravity, key.accountKey)
		log.Infof("usage: purged learned antigravity identity for account %s (profile %s): user-agent does not identify an antigravity client", key.accountKey, key.profileKey)
	}
}

func staleAntigravityFingerprints(db *sql.DB) ([]identityFingerprintKey, error) {
	rows, err := db.Query(`SELECT account_key, profile_key, fields_json FROM identity_fingerprints WHERE tenant_id = ? AND provider = ?`,
		systemTenantID, string(identityfingerprint.ProviderAntigravity))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []identityFingerprintKey
	for rows.Next() {
		var key identityFingerprintKey
		var fieldsJSON string
		if err = rows.Scan(&key.accountKey, &key.profileKey, &fieldsJSON); err != nil {
			return nil, err
		}
		fields := map[string]string{}
		if strings.TrimSpace(fieldsJSON) != "" {
			// A payload that will not parse cannot be shown to hold a real
			// client identity, so it is purged along with the rest.
			_ = json.Unmarshal([]byte(fieldsJSON), &fields)
		}
		if !identityfingerprint.IsAntigravityClientUserAgent(fields[identityfingerprint.FieldUserAgent]) {
			stale = append(stale, key)
		}
	}
	return stale, rows.Err()
}

func migrateIdentityFingerprintTenantSchema(db *sql.DB) {
	if err := migrateIdentityFingerprintTable(db); err != nil {
		log.Errorf("usage: migrate identity_fingerprints tenant primary key: %v", err)
	}
	if err := migrateIdentityFingerprintPolicyTable(db); err != nil {
		log.Errorf("usage: migrate identity_fingerprint_account_policies tenant primary key: %v", err)
	}
}

func migrateIdentityFingerprintTable(db *sql.DB) error {
	pk, err := sqlitePrimaryKeyColumns(db, "identity_fingerprints")
	if err != nil {
		return fmt.Errorf("inspect primary key: %w", err)
	}
	if len(pk) == 4 && pk[0] == "tenant_id" && pk[1] == "provider" && pk[2] == "account_key" && pk[3] == "profile_key" {
		return nil
	}
	if !sqliteColumnExists(db, "identity_fingerprints", "tenant_id") {
		if _, err = db.Exec("ALTER TABLE identity_fingerprints ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '" + systemTenantID + "'"); err != nil {
			return fmt.Errorf("add tenant_id: %w", err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, index := range []string{"idx_identity_fingerprints_provider_seen", "idx_identity_fingerprints_account_seen"} {
		if _, err = tx.Exec("DROP INDEX IF EXISTS " + index); err != nil {
			return fmt.Errorf("drop index %s: %w", index, err)
		}
	}
	if _, err = tx.Exec("ALTER TABLE identity_fingerprints RENAME TO identity_fingerprints_legacy"); err != nil {
		return fmt.Errorf("rename legacy table: %w", err)
	}
	if _, err = tx.Exec(`CREATE TABLE identity_fingerprints (
		tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', provider TEXT NOT NULL,
		account_key TEXT NOT NULL, profile_key TEXT NOT NULL DEFAULT 'default', auth_subject_id TEXT NOT NULL DEFAULT '',
		client_product TEXT NOT NULL DEFAULT '', client_variant TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '',
		fields_json TEXT NOT NULL DEFAULT '{}', observed_headers_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '', last_seen_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (tenant_id, provider, account_key, profile_key))`); err != nil {
		return fmt.Errorf("create tenant table: %w", err)
	}
	columns := "tenant_id,provider,account_key,profile_key,auth_subject_id,client_product,client_variant,version,fields_json,observed_headers_json,created_at,updated_at,last_seen_at"
	if _, err = tx.Exec("INSERT INTO identity_fingerprints(" + columns + ") SELECT " + columns + " FROM identity_fingerprints_legacy"); err != nil {
		return fmt.Errorf("copy legacy rows: %w", err)
	}
	if _, err = tx.Exec("DROP TABLE identity_fingerprints_legacy"); err != nil {
		return fmt.Errorf("drop legacy table: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

func migrateIdentityFingerprintPolicyTable(db *sql.DB) error {
	pk, err := sqlitePrimaryKeyColumns(db, "identity_fingerprint_account_policies")
	if err != nil {
		return fmt.Errorf("inspect primary key: %w", err)
	}
	if len(pk) == 3 && pk[0] == "tenant_id" && pk[1] == "provider" && pk[2] == "account_key" {
		return nil
	}
	if !sqliteColumnExists(db, "identity_fingerprint_account_policies", "tenant_id") {
		if _, err = db.Exec("ALTER TABLE identity_fingerprint_account_policies ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '" + systemTenantID + "'"); err != nil {
			return fmt.Errorf("add tenant_id: %w", err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec("ALTER TABLE identity_fingerprint_account_policies RENAME TO identity_fingerprint_account_policies_legacy"); err != nil {
		return fmt.Errorf("rename legacy table: %w", err)
	}
	if _, err = tx.Exec(`CREATE TABLE identity_fingerprint_account_policies (
		tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', provider TEXT NOT NULL,
		account_key TEXT NOT NULL, strategy TEXT NOT NULL DEFAULT 'cli_preferred', active_profile_key TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, provider, account_key))`); err != nil {
		return fmt.Errorf("create tenant table: %w", err)
	}
	columns := "tenant_id,provider,account_key,strategy,active_profile_key,revision,updated_at"
	if _, err = tx.Exec("INSERT INTO identity_fingerprint_account_policies(" + columns + ") SELECT " + columns + " FROM identity_fingerprint_account_policies_legacy"); err != nil {
		return fmt.Errorf("copy legacy rows: %w", err)
	}
	if _, err = tx.Exec("DROP TABLE identity_fingerprint_account_policies_legacy"); err != nil {
		return fmt.Errorf("drop legacy table: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// ObserveIdentityFingerprint learns/merges fingerprints in the shared AI-account
// catalog. Rows are stored under systemTenantID as an implementation detail; the
// logical key is (provider, account_key, profile_key). The same OAuth account
// keeps one fingerprint set whether its credential currently lives on the
// platform tenant or a business tenant.
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

// GetIdentityFingerprint returns the most recently seen shared fingerprint for
// the AI account (provider + account_key), independent of which business tenant
// currently owns the credential file.
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
		 WHERE tenant_id = ? AND provider = ? AND account_key = ?
		 ORDER BY last_seen_at DESC, profile_key ASC
		 LIMIT 1
	`, systemTenantID, string(provider), accountKey)
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
		 WHERE tenant_id = ? AND provider = ? AND account_key = ? AND profile_key = ?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	record, err := scanIdentityFingerprint(queryer.QueryRow(query, systemTenantID, string(provider), accountKey, profileKey))
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
		 WHERE tenant_id = ? AND provider = ? AND account_key = ?
		 ORDER BY last_seen_at DESC, profile_key ASC
	`, systemTenantID, string(provider), accountKey)
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
		query += ` WHERE tenant_id = ? AND provider = ?`
		args = append(args, systemTenantID, providerText)
	} else {
		query += ` WHERE tenant_id = ?`
		args = append(args, systemTenantID)
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
	conflictTarget := "(tenant_id, provider, account_key, profile_key)"
	_, err = execer.Exec(`
		INSERT INTO identity_fingerprints (
			tenant_id, provider, account_key, profile_key, auth_subject_id, client_product, client_variant, version,
			fields_json, observed_headers_json, created_at, updated_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT`+conflictTarget+` DO UPDATE SET
			auth_subject_id = excluded.auth_subject_id,
			client_product = excluded.client_product,
			client_variant = excluded.client_variant,
			version = excluded.version,
			fields_json = excluded.fields_json,
			observed_headers_json = excluded.observed_headers_json,
			updated_at = excluded.updated_at,
			last_seen_at = excluded.last_seen_at
	`, systemTenantID, provider, accountKey, profileKey, strings.TrimSpace(record.AuthSubjectID), strings.TrimSpace(record.ClientProduct),
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
	res, err := db.Exec(`DELETE FROM identity_fingerprints WHERE tenant_id = ? AND provider = ? AND account_key = ?`, systemTenantID, string(provider), accountKey)
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
	res, err := db.Exec(`DELETE FROM identity_fingerprints WHERE tenant_id = ? AND provider = ? AND account_key = ? AND profile_key = ?`, systemTenantID, string(provider), accountKey, profileKey)
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

package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	log "github.com/sirupsen/logrus"
)

// Entry is one stored runtime setting with its row version.
type Entry struct {
	Payload json.RawMessage
	Version int64
}

// Change is one compare-and-swap of a runtime setting.
type Change struct {
	Key   string
	Value any
	// Expected is the version the new value was computed from: 0 means the
	// row must not exist yet, configsync.AnyVersion skips the check.
	Expected int64
}

// TenantID reports the tenant this store reads and writes.
func (s RuntimeSettingsStore) TenantID() string { return s.tenantID }

// Load returns one stored setting.
func (s RuntimeSettingsStore) Load(key string) (Entry, bool) {
	if s.db == nil {
		return Entry{}, false
	}
	var (
		payload string
		version int64
	)
	err := s.db.QueryRow(`SELECT payload, version FROM runtime_settings WHERE tenant_id = ? AND setting_key = ?`, s.tenantID, key).Scan(&payload, &version)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Warnf("sqlite/settings: load runtime setting %s: %v", key, err)
		}
		return Entry{}, false
	}
	return Entry{Payload: normalizePayload(payload), Version: version}, true
}

// LoadAll returns every stored setting of the tenant in one query, so a
// config is built from one consistent read.
func (s RuntimeSettingsStore) LoadAll() (map[string]Entry, error) {
	if s.db == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT setting_key, payload, version FROM runtime_settings WHERE tenant_id = ?`, s.tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Entry)
	for rows.Next() {
		var (
			key, payload string
			version      int64
		)
		if err := rows.Scan(&key, &payload, &version); err != nil {
			return nil, err
		}
		out[key] = Entry{Payload: normalizePayload(payload), Version: version}
	}
	return out, rows.Err()
}

func normalizePayload(payload string) json.RawMessage {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		payload = "{}"
	}
	return json.RawMessage(payload)
}

// CompareAndSwap writes changes in one transaction. Each key is written only
// if its stored version still equals Change.Expected; one mismatch rolls the
// whole set back and returns a *configsync.ConflictError. Every written key is
// announced to the other nodes inside the same transaction. It returns the new
// version of each key.
func (s RuntimeSettingsStore) CompareAndSwap(ctx context.Context, changes []Change) (map[string]int64, error) {
	if s.db == nil {
		return nil, fmt.Errorf("sqlite/settings: database not initialised")
	}
	if len(changes) == 0 {
		return map[string]int64{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	versions := make(map[string]int64, len(changes))
	events := make([]cluster.ConfigEvent, 0, len(changes))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, change := range changes {
		key := strings.TrimSpace(change.Key)
		if key == "" {
			continue
		}
		payload, err := json.Marshal(change.Value)
		if err != nil {
			return nil, fmt.Errorf("encode runtime setting %s: %w", key, err)
		}
		version, err := s.swapTx(ctx, tx, key, string(payload), now, change.Expected)
		if err != nil {
			return nil, err
		}
		versions[key] = version
		events = append(events, configsync.KeyEvent(configsync.RuntimeSettingDomain(key), s.tenantID, key, version))
	}
	if err := configsync.CommitTx(ctx, tx, events...); err != nil {
		return nil, err
	}
	return versions, nil
}

func (s RuntimeSettingsStore) swapTx(ctx context.Context, tx *sql.Tx, key, payload, now string, expected int64) (int64, error) {
	var (
		version int64
		err     error
	)
	switch {
	case expected < 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO runtime_settings (tenant_id, setting_key, payload, updated_at, version)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT (tenant_id, setting_key) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at,
				version = runtime_settings.version + 1
			RETURNING version`, s.tenantID, key, payload, now).Scan(&version)
	case expected == 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO runtime_settings (tenant_id, setting_key, payload, updated_at, version)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT (tenant_id, setting_key) DO NOTHING
			RETURNING version`, s.tenantID, key, payload, now).Scan(&version)
	default:
		err = tx.QueryRowContext(ctx, `UPDATE runtime_settings SET payload = ?, updated_at = ?, version = version + 1
			WHERE tenant_id = ? AND setting_key = ? AND version = ?
			RETURNING version`, payload, now, s.tenantID, key, expected).Scan(&version)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var current int64
		if scanErr := tx.QueryRowContext(ctx, `SELECT version FROM runtime_settings WHERE tenant_id = ? AND setting_key = ?`, s.tenantID, key).Scan(&current); scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return 0, scanErr
		}
		return 0, &configsync.ConflictError{Domain: configsync.RuntimeSettingDomain(key), TenantID: s.tenantID, Key: key, Expected: expected, Current: current}
	}
	if err != nil {
		return 0, fmt.Errorf("write runtime setting %s: %w", key, err)
	}
	return version, nil
}

// InsertIfAbsent stores value only when the key has no row yet. It reports
// whether it inserted.
func (s RuntimeSettingsStore) InsertIfAbsent(ctx context.Context, key string, value any) (bool, error) {
	_, err := s.CompareAndSwap(ctx, []Change{{Key: key, Value: value, Expected: 0}})
	if errors.Is(err, configsync.ErrVersionConflict) {
		return false, nil
	}
	return err == nil, err
}

// writeIfChanged stores the value spec takes from cfg unless the stored value
// is already equal, retrying when another writer gets in between. It is for
// writes that are not based on a version the client saw.
func (s RuntimeSettingsStore) writeIfChanged(ctx context.Context, spec runtimeconfig.Spec, cfg *config.Config) (bool, error) {
	want, err := runtimeconfig.Canonical(spec, cfg)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		entry, ok := s.Load(spec.Key)
		expected := int64(0)
		if ok {
			if stored, canonErr := CanonicalPayload(spec, entry.Payload); canonErr == nil && bytes.Equal(stored, want) {
				return false, nil
			}
			expected = entry.Version
		}
		_, err = s.CompareAndSwap(ctx, []Change{{Key: spec.Key, Value: spec.Value(cfg), Expected: expected}})
		if !errors.Is(err, configsync.ErrVersionConflict) {
			return err == nil, err
		}
	}
	return false, err
}

// CanonicalPayload renders a stored payload the way Canonical renders a live
// config, so the two can be compared byte for byte.
func CanonicalPayload(spec runtimeconfig.Spec, payload json.RawMessage) ([]byte, error) {
	scratch := &config.Config{}
	if !spec.Apply(scratch, payload) {
		return nil, fmt.Errorf("decode runtime setting %s", spec.Key)
	}
	return runtimeconfig.Canonical(spec, scratch)
}

// RecordSnapshot remembers that cfg holds spec's value at version. A nil state
// records nothing.
func RecordSnapshot(state *config.RuntimeSettingState, spec runtimeconfig.Spec, cfg *config.Config, version int64) {
	if state == nil {
		return
	}
	canonical, err := runtimeconfig.Canonical(spec, cfg)
	if err != nil {
		state.Forget(spec.Key)
		return
	}
	state.Set(spec.Key, config.RuntimeSettingSnapshot{Canonical: canonical, Version: version})
}

// ensureRuntimeSettingsVersionColumn adds the version column to SQLite test
// databases created before it existed. PostgreSQL gets it from a versioned
// migration; there the PRAGMA fails and this returns.
func ensureRuntimeSettingsVersionColumn(db *sql.DB) {
	ensureSQLiteVersionColumn(db, "runtime_settings")
}

// ensureSQLiteVersionColumn adds "version INTEGER NOT NULL DEFAULT 1" to table
// when a SQLite database lacks it.
func ensureSQLiteVersionColumn(db *sql.DB, table string) {
	if db == nil {
		return
	}
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return
	}
	hasVersion, sawColumn := false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if rows.Scan(&cid, &name, &typ, &notNull, &def, &pk) != nil {
			_ = rows.Close()
			return
		}
		sawColumn = true
		hasVersion = hasVersion || name == "version"
	}
	_ = rows.Close()
	if !sawColumn || hasVersion {
		return
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN version INTEGER NOT NULL DEFAULT 1"); err != nil {
		log.Warnf("sqlite/settings: add %s.version: %v", table, err)
	}
}

package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/configsync"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/runtimeconfig"
	log "github.com/sirupsen/logrus"
)

const createRuntimeSettingsTableSQL = `
CREATE TABLE IF NOT EXISTS runtime_settings (
  tenant_id   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  setting_key TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '{}',
  updated_at  TEXT NOT NULL DEFAULT '',
  version     INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, setting_key)
);
`

type RuntimeSettingsStore struct {
	db       *sql.DB
	tenantID string
}

func NewRuntimeSettingsStore(db *sql.DB) RuntimeSettingsStore {
	return NewTenantRuntimeSettingsStore(db, "")
}

func NewTenantRuntimeSettingsStore(db *sql.DB, tenantID string) RuntimeSettingsStore {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000001"
	}
	return RuntimeSettingsStore{db: db, tenantID: tenantID}
}

func InitRuntimeSettingsTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createRuntimeSettingsTableSQL); err != nil {
		log.Errorf("sqlite/settings: create runtime_settings table: %v", err)
	}
	migrateRuntimeSettingsTenantSchema(db)
	ensureRuntimeSettingsVersionColumn(db)
}

func (s RuntimeSettingsStore) Payload(key string) (json.RawMessage, bool) {
	if s.db == nil {
		return nil, false
	}
	var payload string
	if err := s.db.QueryRow(`SELECT payload FROM runtime_settings WHERE tenant_id = $1 AND setting_key = $2`, s.tenantID, key).Scan(&payload); err != nil {
		if err != sql.ErrNoRows {
			log.Warnf("sqlite/settings: load runtime setting %s: %v", key, err)
		}
		return nil, false
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		payload = "{}"
	}
	return json.RawMessage(payload), true
}

func (s RuntimeSettingsStore) Exists(key string) bool {
	_, ok := s.Payload(key)
	return ok
}

// Upsert writes value whatever the stored version is. It still bumps the
// version and announces the write, so readers holding the old version notice.
// Management saves use CompareAndSwap instead.
func (s RuntimeSettingsStore) Upsert(key string, value any) error {
	if s.db == nil {
		return nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	_, err := s.CompareAndSwap(context.Background(), []Change{{Key: key, Value: value, Expected: configsync.AnyVersion}})
	return err
}

// PersistPresentInYAML stores the settings that a raw config.yaml save from
// the management panel spelled out explicitly, in one transaction: either every
// key whose value differs from the stored one is written or none is, so a
// failed save leaves nothing of itself behind for the caller to undo. Each key
// is checked against the version it replaces. The save is not based on a
// version the client saw, so when another writer gets in between, the stored
// values are read again and the set is retried; a conflict is returned only
// once the retries are used up. On success the written keys are returned and
// their new versions recorded in cfg's state.
func (s RuntimeSettingsStore) PersistPresentInYAML(ctx context.Context, cfg *config.Config, yamlContent []byte) ([]string, error) {
	if s.db == nil || cfg == nil {
		return nil, nil
	}
	present := config.YAMLRootKeys(yamlContent)
	if len(present) == 0 {
		return nil, nil
	}
	var wanted []PendingChange
	for _, spec := range runtimeconfig.Specs() {
		if !present[spec.Key] {
			continue
		}
		canonical, err := runtimeconfig.Canonical(spec, cfg)
		if err != nil {
			return nil, fmt.Errorf("encode runtime setting %s: %w", spec.Key, err)
		}
		wanted = append(wanted, PendingChange{Spec: spec, Canonical: canonical})
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var entries map[string]Entry
		if entries, err = s.LoadAll(); err != nil {
			return nil, fmt.Errorf("load runtime settings: %w", err)
		}
		pending := make([]PendingChange, 0, len(wanted))
		changes := make([]Change, 0, len(wanted))
		for _, want := range wanted {
			if entry, ok := entries[want.Spec.Key]; ok {
				if stored, canonErr := CanonicalPayload(want.Spec, entry.Payload); canonErr == nil && bytes.Equal(stored, want.Canonical) {
					continue
				}
				want.Expected = entry.Version
			}
			pending = append(pending, want)
			changes = append(changes, Change{Key: want.Spec.Key, Value: want.Spec.Value(cfg), Expected: want.Expected})
		}
		if len(changes) == 0 {
			return nil, nil
		}
		var versions map[string]int64
		if versions, err = s.CompareAndSwap(ctx, changes); err == nil {
			state := cfg.RuntimeSettingState()
			keys := make([]string, 0, len(pending))
			for _, p := range pending {
				state.Set(p.Spec.Key, config.RuntimeSettingSnapshot{Canonical: p.Canonical, Version: versions[p.Spec.Key]})
				keys = append(keys, p.Spec.Key)
			}
			return keys, nil
		}
		if !errors.Is(err, configsync.ErrVersionConflict) {
			return nil, err
		}
	}
	return nil, err
}

// ApplyToConfig overlays every stored setting onto cfg without recording
// versions, for configs built on the fly (tenant views).
func (s RuntimeSettingsStore) ApplyToConfig(cfg *config.Config) bool {
	applied, _ := s.ApplyToConfigRecording(cfg, nil)
	return applied
}

// ApplyToConfigRecording overlays every stored setting onto cfg and, when state
// is not nil, records what was loaded so a later save can tell which keys it
// changed. It returns the loaded entries.
func (s RuntimeSettingsStore) ApplyToConfigRecording(cfg *config.Config, state *config.RuntimeSettingState) (bool, map[string]Entry) {
	if s.db == nil || cfg == nil {
		return false, nil
	}
	entries, err := s.LoadAll()
	if err != nil {
		log.Warnf("sqlite/settings: load runtime settings: %v", err)
		return false, nil
	}
	applied := false
	for _, spec := range runtimeconfig.Specs() {
		entry, ok := entries[spec.Key]
		if !ok {
			state.Forget(spec.Key)
			continue
		}
		if !spec.Apply(cfg, entry.Payload) {
			continue
		}
		applied = true
		RecordSnapshot(state, spec, cfg, entry.Version)
	}
	return applied, entries
}

// MigrateFromConfig imports the settings the database does not have yet from
// cfg (normally config.yaml). It never overwrites a stored row: when several
// nodes start at once, the first import wins and the others adopt it.
func (s RuntimeSettingsStore) MigrateFromConfig(cfg *config.Config) (migrated int, hadStored bool) {
	if s.db == nil || cfg == nil {
		return 0, false
	}
	entries, err := s.LoadAll()
	if err != nil {
		log.Warnf("sqlite/settings: load runtime settings before migration: %v", err)
		return 0, false
	}
	for _, spec := range runtimeconfig.Specs() {
		if _, ok := entries[spec.Key]; ok {
			hadStored = true
			continue
		}
		if !spec.Meaningful(cfg) {
			continue
		}
		inserted, err := s.InsertIfAbsent(context.Background(), spec.Key, spec.Value(cfg))
		if err != nil {
			log.Errorf("sqlite/settings: migrate runtime setting %s: %v", spec.Key, err)
			continue
		}
		if inserted {
			migrated++
		} else {
			hadStored = true
		}
	}
	return migrated, hadStored
}

func migrateRuntimeSettingsTenantSchema(db *sql.DB) {
	rows, err := db.Query("PRAGMA table_info(runtime_settings)")
	if err != nil {
		return
	}
	defer rows.Close()
	hasTenant, tenantPrimary, keyPrimary := false, false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if rows.Scan(&cid, &name, &typ, &notNull, &def, &pk) != nil {
			return
		}
		hasTenant = hasTenant || name == "tenant_id"
		tenantPrimary = tenantPrimary || (name == "tenant_id" && pk > 0)
		keyPrimary = keyPrimary || (name == "setting_key" && pk > 0)
	}
	_ = rows.Close()
	if hasTenant && tenantPrimary && keyPrimary {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if !hasTenant {
		if _, err = tx.Exec("ALTER TABLE runtime_settings ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil {
			return
		}
	}
	if _, err = tx.Exec("ALTER TABLE runtime_settings RENAME TO runtime_settings_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec(createRuntimeSettingsTableSQL); err != nil {
		return
	}
	if _, err = tx.Exec("INSERT INTO runtime_settings(tenant_id,setting_key,payload,updated_at) SELECT tenant_id,setting_key,payload,updated_at FROM runtime_settings_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec("DROP TABLE runtime_settings_legacy"); err != nil {
		return
	}
	if err = tx.Commit(); err != nil {
		log.Warnf("sqlite/settings: migrate tenant schema: %v", err)
	}
}

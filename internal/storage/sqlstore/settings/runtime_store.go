package settings

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	runtimeconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/runtimeconfig"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const createRuntimeSettingsTableSQL = `
CREATE TABLE IF NOT EXISTS runtime_settings (
  tenant_id   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  setting_key TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '{}',
  updated_at  TEXT NOT NULL DEFAULT '',
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

func (s RuntimeSettingsStore) Upsert(key string, value any) error {
	if s.db == nil {
		return nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO runtime_settings (tenant_id, setting_key, payload, updated_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT(tenant_id, setting_key) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at`,
		s.tenantID,
		key,
		string(payload),
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func (s RuntimeSettingsStore) PersistFromConfig(cfg *config.Config) int {
	if s.db == nil || cfg == nil {
		return 0
	}
	persisted := 0
	for _, spec := range runtimeconfig.Specs() {
		if !spec.Meaningful(cfg) && !s.Exists(spec.Key) {
			continue
		}
		if err := s.Upsert(spec.Key, spec.Value(cfg)); err != nil {
			log.Errorf("sqlite/settings: persist runtime setting %s: %v", spec.Key, err)
			continue
		}
		persisted++
	}
	return persisted
}

func (s RuntimeSettingsStore) PersistPresentInYAML(cfg *config.Config, yamlContent []byte) int {
	if s.db == nil || cfg == nil {
		return 0
	}
	present := yamlRootKeys(yamlContent)
	if len(present) == 0 {
		return 0
	}
	persisted := 0
	for _, spec := range runtimeconfig.Specs() {
		if !present[spec.Key] {
			continue
		}
		if err := s.Upsert(spec.Key, spec.Value(cfg)); err != nil {
			log.Errorf("sqlite/settings: persist runtime setting %s from YAML save: %v", spec.Key, err)
			continue
		}
		persisted++
	}
	return persisted
}

func (s RuntimeSettingsStore) ApplyToConfig(cfg *config.Config) bool {
	if s.db == nil || cfg == nil {
		return false
	}
	applied := false
	for _, spec := range runtimeconfig.Specs() {
		raw, ok := s.Payload(spec.Key)
		if !ok {
			continue
		}
		if spec.Apply(cfg, raw) {
			applied = true
		}
	}
	return applied
}

func (s RuntimeSettingsStore) MigrateFromConfig(cfg *config.Config) (migrated int, hadStored bool) {
	if s.db == nil || cfg == nil {
		return 0, false
	}
	for _, spec := range runtimeconfig.Specs() {
		if s.Exists(spec.Key) {
			hadStored = true
			continue
		}
		if !spec.Meaningful(cfg) {
			continue
		}
		if err := s.Upsert(spec.Key, spec.Value(cfg)); err != nil {
			log.Errorf("sqlite/settings: migrate runtime setting %s: %v", spec.Key, err)
			continue
		}
		migrated++
	}
	return migrated, hadStored
}

func yamlRootKeys(data []byte) map[string]bool {
	if len(data) == 0 {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	mapping := root.Content[0]
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	keys := make(map[string]bool, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key == nil || key.Kind != yaml.ScalarNode {
			continue
		}
		name := strings.TrimSpace(key.Value)
		if name != "" {
			keys[name] = true
		}
	}
	return keys
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

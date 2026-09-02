package modelconfig

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

type ModelConfigRow struct {
	ModelID                   string   `json:"model_id"`
	OwnedBy                   string   `json:"owned_by"`
	DisplayName               string   `json:"display_name,omitempty"`
	Description               string   `json:"description"`
	Enabled                   bool     `json:"enabled"`
	InputModalities           []string `json:"input_modalities,omitempty"`
	OutputModalities          []string `json:"output_modalities,omitempty"`
	ContextLength             int      `json:"context_length,omitempty"`
	MaxCompletionTokens       int      `json:"max_completion_tokens,omitempty"`
	SupportedParameters       []string `json:"supported_parameters,omitempty"`
	Reasoning                 string   `json:"reasoning,omitempty"`
	KnowledgeCutoff           string   `json:"knowledge_cutoff,omitempty"`
	PricingMode               string   `json:"pricing_mode"`
	InputPricePerMillion      float64  `json:"input_price_per_million"`
	OutputPricePerMillion     float64  `json:"output_price_per_million"`
	CachedPricePerMillion     float64  `json:"cached_price_per_million"`
	CacheReadPricePerMillion  float64  `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion float64  `json:"cache_write_price_per_million,omitempty"`
	PricePerCall              float64  `json:"price_per_call"`
	Source                    string   `json:"source"`
	UpdatedAt                 string   `json:"updated_at"`
}

type ModelOwnerPresetRow struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	UpdatedAt   string `json:"updated_at"`
}

const createModelConfigTablesSQL = `
CREATE TABLE IF NOT EXISTS model_configs (
  tenant_id                     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  model_id                      TEXT NOT NULL,
  owned_by                      TEXT NOT NULL DEFAULT '',
  display_name                  TEXT NOT NULL DEFAULT '',
  description                   TEXT NOT NULL DEFAULT '',
  enabled                       INTEGER NOT NULL DEFAULT 1,
  input_modalities              TEXT NOT NULL DEFAULT '',
  output_modalities             TEXT NOT NULL DEFAULT '',
  context_length                INTEGER NOT NULL DEFAULT 0,
  max_completion_tokens         INTEGER NOT NULL DEFAULT 0,
  supported_parameters          TEXT NOT NULL DEFAULT '',
  reasoning                     TEXT NOT NULL DEFAULT '',
  knowledge_cutoff              TEXT NOT NULL DEFAULT '',
  pricing_mode                  TEXT NOT NULL DEFAULT 'token',
  input_price_per_million        REAL NOT NULL DEFAULT 0,
  output_price_per_million       REAL NOT NULL DEFAULT 0,
  cached_price_per_million       REAL NOT NULL DEFAULT 0,
  cache_read_price_per_million   REAL NOT NULL DEFAULT 0,
  cache_write_price_per_million  REAL NOT NULL DEFAULT 0,
  price_per_call                 REAL NOT NULL DEFAULT 0,
  source                        TEXT NOT NULL DEFAULT 'user',
  updated_at                    DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, model_id)
);

CREATE INDEX IF NOT EXISTS idx_model_configs_owned_by ON model_configs(owned_by);

CREATE TABLE IF NOT EXISTS model_owner_presets (
  tenant_id   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  value       TEXT NOT NULL,
  label       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  updated_at  DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, value)
);

CREATE TABLE IF NOT EXISTS auth_group_model_owner_mappings (
  tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  auth_group TEXT NOT NULL,
  owner      TEXT NOT NULL DEFAULT '',
  updated_at DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, auth_group)
);

CREATE INDEX IF NOT EXISTS idx_auth_group_model_owner_mappings_owner
  ON auth_group_model_owner_mappings(owner);

CREATE TABLE IF NOT EXISTS model_openrouter_sync_state (
  tenant_id        TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  id               INTEGER NOT NULL CHECK(id = 1),
  enabled          INTEGER NOT NULL DEFAULT 0,
  interval_minutes INTEGER NOT NULL DEFAULT 1440,
  last_sync_at     TEXT NOT NULL DEFAULT '',
  last_success_at  TEXT NOT NULL DEFAULT '',
  last_error       TEXT NOT NULL DEFAULT '',
  last_seen        INTEGER NOT NULL DEFAULT 0,
  last_added       INTEGER NOT NULL DEFAULT 0,
  last_updated     INTEGER NOT NULL DEFAULT 0,
  last_skipped     INTEGER NOT NULL DEFAULT 0,
  updated_at       DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
`

var defaultOwnerLabels = map[string]string{
	"anthropic":    "Anthropic",
	"openai":       "OpenAI",
	"google":       "Google",
	"gemini":       "Gemini",
	"vertex":       "Vertex AI",
	"deepseek":     "DeepSeek",
	"qwen":         "Qwen",
	"kimi":         "Kimi",
	"minimax":      "MiniMax",
	"grok":         "Grok",
	"glm":          "GLM",
	"codex":        "Codex",
	"iflow":        "iFlow",
	"kiro":         "Kiro",
	"openrouter":   "OpenRouter",
	"azure-openai": "Azure OpenAI",
}

type Store struct {
	db       *sql.DB
	tenantID string
}

func NewStore(db *sql.DB) Store {
	return NewTenantStore(db, "")
}

func NewTenantStore(db *sql.DB, tenantID string) Store {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000001"
	}
	return Store{db: db, tenantID: tenantID}
}

func InitTables(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createModelConfigTablesSQL); err != nil {
		log.Errorf("sqlite/modelconfig: create model config tables: %v", err)
		return
	}
	ensureModelConfigSchema(db)
	ensureOpenRouterModelSyncStateSchema(db)
	migrateModelConfigTenantSchema(db)
	seedDefaultModelConfigRows(db)
	mergeLegacyPricingIntoModelConfigs(db)
	repairDefaultPerCallModelConfigRows(db)
	repairMediaGenerationModelConfigRows(db)
}

func NormalizeModelOwnerValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), "-"))
}

func NormalizePricingMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "call") {
		return "call"
	}
	return "token"
}

func NormalizeModelModalities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		modality := strings.ToLower(strings.TrimSpace(value))
		if modality == "" {
			continue
		}
		if _, ok := seen[modality]; ok {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	return out
}

func OwnerLabelForValue(value string) string {
	value = NormalizeModelOwnerValue(value)
	if label := defaultOwnerLabels[value]; label != "" {
		return label
	}
	parts := strings.Split(value, "-")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func UpsertLegacyPricingIntoModelConfig(db *sql.DB, modelID string, input, output, cached float64, updatedAt string) {
	UpsertLegacyPricingIntoModelConfigForTenant(db, "00000000-0000-0000-0000-000000000001", modelID, input, output, cached, updatedAt)
}

func UpsertLegacyPricingIntoModelConfigForTenant(db *sql.DB, tenantID, modelID string, input, output, cached float64, updatedAt string) {
	if db == nil {
		return
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	_, err := db.Exec(
		`INSERT INTO model_configs
		 (tenant_id, model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
		 VALUES (?, ?, '', '', 1, 'token', ?, ?, ?, 0, 'legacy-pricing', ?)
		 ON CONFLICT(tenant_id, model_id) DO UPDATE SET
		   pricing_mode = 'token',
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   price_per_call = 0,
		   updated_at = excluded.updated_at`,
		tenantID,
		modelID,
		input,
		output,
		cached,
		updatedAt,
	)
	if err != nil {
		log.Warnf("sqlite/modelconfig: sync legacy pricing into model config %s: %v", modelID, err)
		return
	}
}

func (s Store) ListModelConfigs() []ModelConfigRow {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT model_id, owned_by, display_name, description, enabled, input_modalities, output_modalities, context_length, max_completion_tokens, supported_parameters, reasoning, knowledge_cutoff, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, price_per_call, source, updated_at FROM model_configs WHERE tenant_id = ? ORDER BY lower(model_id)`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/modelconfig: list tenant model configs: %v", err)
		return nil
	}
	defer rows.Close()
	result := make([]ModelConfigRow, 0)
	for rows.Next() {
		row, ok := scanModelConfigRow(rows)
		if ok {
			result = append(result, row)
		}
	}
	return result
}

func (s Store) GetModelConfig(modelID string) (ModelConfigRow, bool) {
	if s.db == nil {
		return ModelConfigRow{}, false
	}
	return scanModelConfigRow(s.db.QueryRow(`SELECT model_id, owned_by, display_name, description, enabled, input_modalities, output_modalities, context_length, max_completion_tokens, supported_parameters, reasoning, knowledge_cutoff, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, price_per_call, source, updated_at FROM model_configs WHERE tenant_id = ? AND model_id = ?`, s.tenantID, strings.TrimSpace(modelID)))
}

func (s Store) UpsertModelConfig(row ModelConfigRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	row.ModelID = strings.TrimSpace(row.ModelID)
	if row.ModelID == "" {
		return fmt.Errorf("model id is required")
	}
	row.OwnedBy = NormalizeModelOwnerValue(row.OwnedBy)
	row.InputModalities = NormalizeModelModalities(row.InputModalities)
	row.OutputModalities = NormalizeModelModalities(row.OutputModalities)
	row.PricingMode = NormalizePricingMode(row.PricingMode)
	if row.Source == "" {
		row.Source = "user"
	}
	row.UpdatedAt = nowRFC3339()
	row.DisplayName = strings.TrimSpace(row.DisplayName)
	row.KnowledgeCutoff = strings.TrimSpace(row.KnowledgeCutoff)
	row.SupportedParameters = NormalizeModelParameterNames(row.SupportedParameters)
	row.Reasoning = NormalizeModelReasoningJSON(row.Reasoning)
	_, err := s.db.Exec(
		`INSERT INTO model_configs
		 (tenant_id, model_id, owned_by, display_name, description, enabled, input_modalities, output_modalities, context_length, max_completion_tokens, supported_parameters, reasoning, knowledge_cutoff, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, price_per_call, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, model_id) DO UPDATE SET
		   owned_by = excluded.owned_by,
		   display_name = excluded.display_name,
		   description = excluded.description,
		   enabled = excluded.enabled,
		   input_modalities = excluded.input_modalities,
		   output_modalities = excluded.output_modalities,
		   context_length = excluded.context_length,
		   max_completion_tokens = excluded.max_completion_tokens,
		   supported_parameters = excluded.supported_parameters,
		   reasoning = excluded.reasoning,
		   knowledge_cutoff = excluded.knowledge_cutoff,
		   pricing_mode = excluded.pricing_mode,
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   cache_read_price_per_million = excluded.cache_read_price_per_million,
		   cache_write_price_per_million = excluded.cache_write_price_per_million,
		   price_per_call = excluded.price_per_call,
		   source = excluded.source,
		   updated_at = excluded.updated_at`,
		s.tenantID,
		row.ModelID,
		row.OwnedBy,
		row.DisplayName,
		row.Description,
		boolToInt(row.Enabled),
		encodeModelModalities(row.InputModalities),
		encodeModelModalities(row.OutputModalities),
		row.ContextLength,
		row.MaxCompletionTokens,
		encodeModelModalities(row.SupportedParameters),
		row.Reasoning,
		row.KnowledgeCutoff,
		row.PricingMode,
		row.InputPricePerMillion,
		row.OutputPricePerMillion,
		row.CachedPricePerMillion,
		row.CacheReadPricePerMillion,
		row.CacheWritePricePerMillion,
		row.PricePerCall,
		row.Source,
		row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert model config: %w", err)
	}
	return nil
}

func (s Store) DeleteModelConfig(modelID string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("model id is required")
	}
	if _, err := s.db.Exec("DELETE FROM model_configs WHERE tenant_id = ? AND model_id = ?", s.tenantID, modelID); err != nil {
		return fmt.Errorf("delete model config: %w", err)
	}
	return nil
}

func (s Store) ListModelOwnerPresets() []ModelOwnerPresetRow {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT value,label,description,enabled,updated_at FROM model_owner_presets WHERE tenant_id = ? ORDER BY lower(value)`, s.tenantID)
	if err != nil {
		log.Errorf("sqlite/modelconfig: list tenant owner presets: %v", err)
		return nil
	}
	defer rows.Close()
	result := make([]ModelOwnerPresetRow, 0)
	for rows.Next() {
		var row ModelOwnerPresetRow
		var enabled int
		if rows.Scan(&row.Value, &row.Label, &row.Description, &enabled, &row.UpdatedAt) == nil {
			row.Enabled = intToBool(enabled)
			result = append(result, row)
		}
	}
	return result
}

func (s Store) GetModelOwnerPreset(value string) (ModelOwnerPresetRow, bool) {
	var row ModelOwnerPresetRow
	var enabled int
	if s.db == nil {
		return row, false
	}
	err := s.db.QueryRow(`SELECT value,label,description,enabled,updated_at FROM model_owner_presets WHERE tenant_id = ? AND value = ?`, s.tenantID, NormalizeModelOwnerValue(value)).Scan(&row.Value, &row.Label, &row.Description, &enabled, &row.UpdatedAt)
	if err != nil {
		return ModelOwnerPresetRow{}, false
	}
	row.Enabled = intToBool(enabled)
	return row, true
}

func (s Store) UpsertModelOwnerPreset(row ModelOwnerPresetRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	row.Value = NormalizeModelOwnerValue(row.Value)
	if row.Value == "" {
		return fmt.Errorf("owner value is required")
	}
	if strings.TrimSpace(row.Label) == "" {
		row.Label = OwnerLabelForValue(row.Value)
	}
	row.UpdatedAt = nowRFC3339()
	_, err := s.db.Exec(
		`INSERT INTO model_owner_presets (tenant_id, value, label, description, enabled, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, value) DO UPDATE SET
		   label = excluded.label,
		   description = excluded.description,
		   enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		s.tenantID,
		row.Value,
		row.Label,
		row.Description,
		boolToInt(row.Enabled),
		row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert owner preset: %w", err)
	}
	return nil
}

func (s Store) ReplaceModelOwnerPresets(rows []ModelOwnerPresetRow) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin owner preset replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM model_owner_presets WHERE tenant_id = ?", s.tenantID); err != nil {
		return fmt.Errorf("clear owner presets: %w", err)
	}
	now := nowRFC3339()
	for _, row := range rows {
		row.Value = NormalizeModelOwnerValue(row.Value)
		if row.Value == "" {
			continue
		}
		if strings.TrimSpace(row.Label) == "" {
			row.Label = OwnerLabelForValue(row.Value)
		}
		if _, err := tx.Exec(
			`INSERT INTO model_owner_presets (tenant_id, value, label, description, enabled, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			s.tenantID,
			row.Value,
			row.Label,
			row.Description,
			boolToInt(row.Enabled),
			now,
		); err != nil {
			return fmt.Errorf("insert owner preset: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit owner preset replace: %w", err)
	}
	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intToBool(value int) bool {
	return value != 0
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func migrateModelConfigTenantSchema(db *sql.DB) {
	if db == nil || !sqliteColumnExists(db, "model_configs", "model_id") {
		return
	}
	tables := []struct {
		name, create, columns string
	}{
		{"model_configs", `CREATE TABLE model_configs (tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', model_id TEXT NOT NULL, owned_by TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, input_modalities TEXT NOT NULL DEFAULT '', output_modalities TEXT NOT NULL DEFAULT '', context_length INTEGER NOT NULL DEFAULT 0, max_completion_tokens INTEGER NOT NULL DEFAULT 0, supported_parameters TEXT NOT NULL DEFAULT '', reasoning TEXT NOT NULL DEFAULT '', knowledge_cutoff TEXT NOT NULL DEFAULT '', pricing_mode TEXT NOT NULL DEFAULT 'token', input_price_per_million REAL NOT NULL DEFAULT 0, output_price_per_million REAL NOT NULL DEFAULT 0, cached_price_per_million REAL NOT NULL DEFAULT 0, cache_read_price_per_million REAL NOT NULL DEFAULT 0, cache_write_price_per_million REAL NOT NULL DEFAULT 0, price_per_call REAL NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT 'user', updated_at DATETIME NOT NULL, PRIMARY KEY (tenant_id, model_id))`, "tenant_id,model_id,owned_by,description,enabled,input_modalities,output_modalities,pricing_mode,input_price_per_million,output_price_per_million,cached_price_per_million,cache_read_price_per_million,cache_write_price_per_million,price_per_call,source,updated_at"},
		{"model_owner_presets", `CREATE TABLE model_owner_presets (tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', value TEXT NOT NULL, label TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL, PRIMARY KEY (tenant_id, value))`, "tenant_id,value,label,description,enabled,updated_at"},
		{"auth_group_model_owner_mappings", `CREATE TABLE auth_group_model_owner_mappings (tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', auth_group TEXT NOT NULL, owner TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL, PRIMARY KEY (tenant_id, auth_group))`, "tenant_id,auth_group,owner,updated_at"},
		{"model_openrouter_sync_state", `CREATE TABLE model_openrouter_sync_state (tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001', id INTEGER NOT NULL CHECK(id = 1), enabled INTEGER NOT NULL DEFAULT 0, interval_minutes INTEGER NOT NULL DEFAULT 1440, last_sync_at TEXT NOT NULL DEFAULT '', last_success_at TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '', last_seen INTEGER NOT NULL DEFAULT 0, last_added INTEGER NOT NULL DEFAULT 0, last_updated INTEGER NOT NULL DEFAULT 0, last_skipped INTEGER NOT NULL DEFAULT 0, updated_at DATETIME NOT NULL, PRIMARY KEY (tenant_id, id))`, "tenant_id,id,enabled,interval_minutes,last_sync_at,last_success_at,last_error,last_seen,last_added,last_updated,last_skipped,updated_at"},
	}
	for _, table := range tables {
		if !sqliteColumnExists(db, table.name, strings.Split(table.columns, ",")[1]) {
			continue
		}
		if sqliteCompositeTenantPrimaryKey(db, table.name) {
			continue
		}
		if err := rebuildTenantTable(db, table.name, table.create, table.columns); err != nil {
			log.Warnf("sqlite/modelconfig: migrate %s tenant schema: %v", table.name, err)
		}
	}
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_model_configs_owned_by ON model_configs(tenant_id, owned_by)")
	_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_auth_group_model_owner_mappings_owner ON auth_group_model_owner_mappings(tenant_id, owner)")
}

func sqliteCompositeTenantPrimaryKey(db *sql.DB, table string) bool {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return true
	}
	defer rows.Close()
	tenantPK, otherPK := false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if rows.Scan(&cid, &name, &typ, &notNull, &def, &pk) != nil {
			return true
		}
		tenantPK = tenantPK || (name == "tenant_id" && pk > 0)
		otherPK = otherPK || (name != "tenant_id" && pk > 0)
	}
	return tenantPK && otherPK
}

func rebuildTenantTable(db *sql.DB, table, createSQL, columns string) error {
	hasTenant := sqliteColumnExists(db, table, "tenant_id")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if !hasTenant {
		if _, err = tx.Exec("ALTER TABLE " + table + " ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil {
			return err
		}
	}
	legacy := table + "_legacy"
	if _, err = tx.Exec("ALTER TABLE " + table + " RENAME TO " + legacy); err != nil {
		return err
	}
	if _, err = tx.Exec(createSQL); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO " + table + "(" + columns + ") SELECT " + columns + " FROM " + legacy); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TABLE " + legacy); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureModelConfigSchema(db *sql.DB) {
	if db == nil {
		return
	}
	textColumns := []string{
		"display_name",
		"input_modalities",
		"output_modalities",
		"supported_parameters",
		"reasoning",
		"knowledge_cutoff",
	}
	for _, col := range textColumns {
		if !sqliteColumnExists(db, "model_configs", col) {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE model_configs ADD COLUMN %s TEXT NOT NULL DEFAULT ''", col)); err != nil {
				log.Warnf("sqlite/modelconfig: add model config column %s: %v", col, err)
			}
		}
	}
	intColumns := []string{"context_length", "max_completion_tokens"}
	for _, col := range intColumns {
		if !sqliteColumnExists(db, "model_configs", col) {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE model_configs ADD COLUMN %s INTEGER NOT NULL DEFAULT 0", col)); err != nil {
				log.Warnf("sqlite/modelconfig: add model config column %s: %v", col, err)
			}
		}
	}
	for _, col := range []string{"cache_read_price_per_million", "cache_write_price_per_million"} {
		if !sqliteColumnExists(db, "model_configs", col) {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE model_configs ADD COLUMN %s REAL NOT NULL DEFAULT 0", col)); err != nil {
				log.Warnf("sqlite/modelconfig: add model config column %s: %v", col, err)
			}
		}
	}
}

func ensureOpenRouterModelSyncStateSchema(db *sql.DB) {
	if db == nil || sqliteColumnExists(db, "model_openrouter_sync_state", "last_updated") {
		return
	}
	if _, err := db.Exec("ALTER TABLE model_openrouter_sync_state ADD COLUMN last_updated INTEGER NOT NULL DEFAULT 0"); err != nil {
		log.Warnf("sqlite/modelconfig: add openrouter sync last_updated column: %v", err)
	}
}

func encodeModelModalities(values []string) string {
	values = NormalizeModelModalities(values)
	if len(values) == 0 {
		return ""
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decodeModelModalities(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil
	}
	return NormalizeModelModalities(values)
}

func NormalizeModelParameterNames(values []string) []string {
	return NormalizeModelModalities(values)
}

func NormalizeModelReasoningJSON(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var payload any
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return ""
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func defaultModelConfigRows() []ModelConfigRow {
	channels := seedModelChannels()

	seen := make(map[string]struct{})
	rows := make([]ModelConfigRow, 0, 256)
	for _, channel := range channels {
		for _, model := range registry.GetStaticModelDefinitionsByChannel(channel) {
			if model == nil || strings.TrimSpace(model.ID) == "" {
				continue
			}
			modelID := strings.TrimSpace(model.ID)
			if _, ok := seen[modelID]; ok {
				continue
			}
			seen[modelID] = struct{}{}

			ownedBy := NormalizeModelOwnerValue(model.OwnedBy)
			if ownedBy == "" {
				ownedBy = NormalizeModelOwnerValue(model.Type)
			}
			if ownedBy == "" {
				ownedBy = NormalizeModelOwnerValue(channel)
			}
			description := strings.TrimSpace(model.Description)
			if description == "" {
				description = strings.TrimSpace(model.DisplayName)
			}

			row := ModelConfigRow{
				ModelID:             modelID,
				OwnedBy:             ownedBy,
				DisplayName:         strings.TrimSpace(model.DisplayName),
				Description:         description,
				Enabled:             true,
				ContextLength:       model.ContextLength,
				MaxCompletionTokens: model.MaxCompletionTokens,
				SupportedParameters: append([]string(nil), model.SupportedParameters...),
				PricingMode:         "token",
				Source:              "seed",
			}
			applyMediaGenerationSeedDefaults(&row, modelID)
			rows = append(rows, row)
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].ModelID) < strings.ToLower(rows[j].ModelID)
	})
	return rows
}

func seedDefaultModelConfigRows(db *sql.DB) {
	now := nowRFC3339()
	for _, row := range defaultModelConfigRows() {
		_, err := db.Exec(
			`INSERT OR IGNORE INTO model_configs
			 (tenant_id, model_id, owned_by, description, enabled, input_modalities, output_modalities, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"00000000-0000-0000-0000-000000000001",
			row.ModelID,
			row.OwnedBy,
			row.Description,
			boolToInt(row.Enabled),
			encodeModelModalities(row.InputModalities),
			encodeModelModalities(row.OutputModalities),
			NormalizePricingMode(row.PricingMode),
			row.InputPricePerMillion,
			row.OutputPricePerMillion,
			row.CachedPricePerMillion,
			row.PricePerCall,
			row.Source,
			now,
		)
		if err != nil {
			log.Warnf("sqlite/modelconfig: seed model config %s: %v", row.ModelID, err)
		}
	}

	for value, label := range defaultOwnerLabels {
		_, err := db.Exec(
			`INSERT OR IGNORE INTO model_owner_presets (tenant_id, value, label, description, enabled, updated_at)
			 VALUES (?, ?, ?, '', 1, ?)`,
			"00000000-0000-0000-0000-000000000001",
			value,
			label,
			now,
		)
		if err != nil {
			log.Warnf("sqlite/modelconfig: seed owner preset %s: %v", value, err)
		}
	}

	rows, err := db.Query("SELECT DISTINCT owned_by FROM model_configs WHERE tenant_id = '00000000-0000-0000-0000-000000000001' AND owned_by != ''")
	if err != nil {
		log.Warnf("sqlite/modelconfig: seed owner presets from model configs: %v", err)
		return
	}
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			continue
		}
		owners = append(owners, owner)
	}
	_ = rows.Close()

	for _, owner := range owners {
		value := NormalizeModelOwnerValue(owner)
		if value == "" {
			continue
		}
		label := defaultOwnerLabels[value]
		if label == "" {
			label = owner
		}
		_, _ = db.Exec(
			`INSERT OR IGNORE INTO model_owner_presets (tenant_id, value, label, description, enabled, updated_at)
			 VALUES (?, ?, ?, '', 1, ?)`,
			"00000000-0000-0000-0000-000000000001",
			value,
			label,
			now,
		)
	}
}

func mergeLegacyPricingIntoModelConfigs(db *sql.DB) {
	rows, err := db.Query("SELECT model_id, input_price_per_million, output_price_per_million, cached_price_per_million FROM model_pricing")
	if err != nil {
		return
	}

	type legacyPricingRow struct {
		modelID string
		input   float64
		output  float64
		cached  float64
	}

	legacyRows := make([]legacyPricingRow, 0)
	for rows.Next() {
		var row legacyPricingRow
		if err := rows.Scan(&row.modelID, &row.input, &row.output, &row.cached); err != nil {
			continue
		}
		row.modelID = strings.TrimSpace(row.modelID)
		if row.modelID == "" {
			continue
		}
		legacyRows = append(legacyRows, row)
	}
	_ = rows.Close()

	now := nowRFC3339()
	for _, row := range legacyRows {
		_, _ = db.Exec(
			`INSERT INTO model_configs
			 (tenant_id, model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
			 VALUES (?, ?, '', '', 1, 'token', ?, ?, ?, 0, 'legacy-pricing', ?)
			 ON CONFLICT(tenant_id, model_id) DO UPDATE SET
			   pricing_mode = 'token',
			   input_price_per_million = excluded.input_price_per_million,
			   output_price_per_million = excluded.output_price_per_million,
			   cached_price_per_million = excluded.cached_price_per_million,
			   updated_at = excluded.updated_at`,
			"00000000-0000-0000-0000-000000000001",
			row.modelID,
			row.input,
			row.output,
			row.cached,
			now,
		)
	}
}

func repairDefaultPerCallModelConfigRows(db *sql.DB) {
	now := nowRFC3339()
	for _, row := range defaultModelConfigRows() {
		if NormalizePricingMode(row.PricingMode) != "call" || row.PricePerCall <= 0 {
			continue
		}
		inputModalities := encodeModelModalities(row.InputModalities)
		outputModalities := encodeModelModalities(row.OutputModalities)
		_, err := db.Exec(
			`UPDATE model_configs
			 SET input_modalities = ?,
			     output_modalities = ?,
			     pricing_mode = 'call',
			     input_price_per_million = 0,
			     output_price_per_million = 0,
			     cached_price_per_million = 0,
			     cache_read_price_per_million = 0,
			     cache_write_price_per_million = 0,
			     price_per_call = ?,
			     updated_at = ?
			 WHERE tenant_id = '00000000-0000-0000-0000-000000000001' AND model_id = ?
			   AND source IN ('seed', 'openrouter', 'legacy-pricing')
			   AND (
			     pricing_mode != 'call'
			     OR input_price_per_million != 0
			     OR output_price_per_million != 0
			     OR cached_price_per_million != 0
			     OR cache_read_price_per_million != 0
			     OR cache_write_price_per_million != 0
			     OR price_per_call <= 0
			     OR input_modalities != ?
			     OR output_modalities != ?
			   )`,
			inputModalities,
			outputModalities,
			row.PricePerCall,
			now,
			row.ModelID,
			inputModalities,
			outputModalities,
		)
		if err != nil {
			log.Warnf("sqlite/modelconfig: repair default per-call model config %s: %v", row.ModelID, err)
		}
	}
}

func scanModelConfigRow(scanner interface{ Scan(...any) error }) (ModelConfigRow, bool) {
	var row ModelConfigRow
	var enabled int
	var inputModalities, outputModalities, supportedParameters string
	err := scanner.Scan(
		&row.ModelID,
		&row.OwnedBy,
		&row.DisplayName,
		&row.Description,
		&enabled,
		&inputModalities,
		&outputModalities,
		&row.ContextLength,
		&row.MaxCompletionTokens,
		&supportedParameters,
		&row.Reasoning,
		&row.KnowledgeCutoff,
		&row.PricingMode,
		&row.InputPricePerMillion,
		&row.OutputPricePerMillion,
		&row.CachedPricePerMillion,
		&row.CacheReadPricePerMillion,
		&row.CacheWritePricePerMillion,
		&row.PricePerCall,
		&row.Source,
		&row.UpdatedAt,
	)
	if err != nil {
		return ModelConfigRow{}, false
	}
	row.Enabled = intToBool(enabled)
	row.InputModalities = decodeModelModalities(inputModalities)
	row.OutputModalities = decodeModelModalities(outputModalities)
	row.SupportedParameters = decodeModelModalities(supportedParameters)
	row.Reasoning = NormalizeModelReasoningJSON(row.Reasoning)
	row.PricingMode = NormalizePricingMode(row.PricingMode)
	return row, true
}

func sqliteColumnExists(db *sql.DB, tableName, columnName string) bool {
	rows, err := db.Query("PRAGMA table_info(" + tableName + ")")
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			continue
		}
		if name == columnName {
			return true
		}
	}
	return false
}

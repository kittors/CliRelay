package modelconfig

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

type ModelConfigRow struct {
	ModelID                   string   `json:"model_id"`
	OwnedBy                   string   `json:"owned_by"`
	Description               string   `json:"description"`
	Enabled                   bool     `json:"enabled"`
	InputModalities           []string `json:"input_modalities,omitempty"`
	OutputModalities          []string `json:"output_modalities,omitempty"`
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
  model_id                      TEXT PRIMARY KEY,
  owned_by                      TEXT NOT NULL DEFAULT '',
  description                   TEXT NOT NULL DEFAULT '',
  enabled                       INTEGER NOT NULL DEFAULT 1,
  input_modalities              TEXT NOT NULL DEFAULT '',
  output_modalities             TEXT NOT NULL DEFAULT '',
  pricing_mode                  TEXT NOT NULL DEFAULT 'token',
  input_price_per_million        REAL NOT NULL DEFAULT 0,
  output_price_per_million       REAL NOT NULL DEFAULT 0,
  cached_price_per_million       REAL NOT NULL DEFAULT 0,
  cache_read_price_per_million   REAL NOT NULL DEFAULT 0,
  cache_write_price_per_million  REAL NOT NULL DEFAULT 0,
  price_per_call                 REAL NOT NULL DEFAULT 0,
  source                        TEXT NOT NULL DEFAULT 'user',
  updated_at                    DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_model_configs_owned_by ON model_configs(owned_by);

CREATE TABLE IF NOT EXISTS model_owner_presets (
  value       TEXT PRIMARY KEY,
  label       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  updated_at  DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS auth_group_model_owner_mappings (
  auth_group TEXT PRIMARY KEY,
  owner      TEXT NOT NULL DEFAULT '',
  updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auth_group_model_owner_mappings_owner
  ON auth_group_model_owner_mappings(owner);

CREATE TABLE IF NOT EXISTS model_openrouter_sync_state (
  id               INTEGER PRIMARY KEY CHECK(id = 1),
  enabled          INTEGER NOT NULL DEFAULT 0,
  interval_minutes INTEGER NOT NULL DEFAULT 1440,
  last_sync_at     TEXT NOT NULL DEFAULT '',
  last_success_at  TEXT NOT NULL DEFAULT '',
  last_error       TEXT NOT NULL DEFAULT '',
  last_seen        INTEGER NOT NULL DEFAULT 0,
  last_added       INTEGER NOT NULL DEFAULT 0,
  last_updated     INTEGER NOT NULL DEFAULT 0,
  last_skipped     INTEGER NOT NULL DEFAULT 0,
  updated_at       DATETIME NOT NULL
);
`

var (
	modelConfigCache   map[string]ModelConfigRow
	modelConfigCacheMu sync.RWMutex

	modelOwnerPresetCache   map[string]ModelOwnerPresetRow
	modelOwnerPresetCacheMu sync.RWMutex
)

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
	db *sql.DB
}

func NewStore(db *sql.DB) Store {
	return Store{db: db}
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
	seedDefaultModelConfigRows(db)
	mergeLegacyPricingIntoModelConfigs(db)
	repairDefaultPerCallModelConfigRows(db)
	reloadModelConfigCache(db)
	reloadModelOwnerPresetCache(db)
	reloadAuthGroupOwnerMappingCache(db)
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
	if db == nil {
		return
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	_, err := db.Exec(
		`INSERT INTO model_configs
		 (model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
		 VALUES (?, '', '', 1, 'token', ?, ?, ?, 0, 'legacy-pricing', ?)
		 ON CONFLICT(model_id) DO UPDATE SET
		   pricing_mode = 'token',
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   price_per_call = 0,
		   updated_at = excluded.updated_at`,
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
	reloadModelConfigCache(db)
}

func (s Store) ListModelConfigs() []ModelConfigRow {
	modelConfigCacheMu.RLock()
	defer modelConfigCacheMu.RUnlock()
	result := make([]ModelConfigRow, 0, len(modelConfigCache))
	for _, row := range modelConfigCache {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].ModelID) < strings.ToLower(result[j].ModelID)
	})
	return result
}

func (s Store) GetModelConfig(modelID string) (ModelConfigRow, bool) {
	modelConfigCacheMu.RLock()
	defer modelConfigCacheMu.RUnlock()
	row, ok := modelConfigCache[strings.TrimSpace(modelID)]
	return row, ok
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
	_, err := s.db.Exec(
		`INSERT INTO model_configs
		 (model_id, owned_by, description, enabled, input_modalities, output_modalities, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, price_per_call, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET
		   owned_by = excluded.owned_by,
		   description = excluded.description,
		   enabled = excluded.enabled,
		   input_modalities = excluded.input_modalities,
		   output_modalities = excluded.output_modalities,
		   pricing_mode = excluded.pricing_mode,
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   cache_read_price_per_million = excluded.cache_read_price_per_million,
		   cache_write_price_per_million = excluded.cache_write_price_per_million,
		   price_per_call = excluded.price_per_call,
		   source = excluded.source,
		   updated_at = excluded.updated_at`,
		row.ModelID,
		row.OwnedBy,
		row.Description,
		boolToInt(row.Enabled),
		encodeModelModalities(row.InputModalities),
		encodeModelModalities(row.OutputModalities),
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
	reloadModelConfigCache(s.db)
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
	if _, err := s.db.Exec("DELETE FROM model_configs WHERE model_id = ?", modelID); err != nil {
		return fmt.Errorf("delete model config: %w", err)
	}
	reloadModelConfigCache(s.db)
	return nil
}

func (s Store) ListModelOwnerPresets() []ModelOwnerPresetRow {
	modelOwnerPresetCacheMu.RLock()
	defer modelOwnerPresetCacheMu.RUnlock()
	result := make([]ModelOwnerPresetRow, 0, len(modelOwnerPresetCache))
	for _, row := range modelOwnerPresetCache {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Value) < strings.ToLower(result[j].Value)
	})
	return result
}

func (s Store) GetModelOwnerPreset(value string) (ModelOwnerPresetRow, bool) {
	modelOwnerPresetCacheMu.RLock()
	defer modelOwnerPresetCacheMu.RUnlock()
	row, ok := modelOwnerPresetCache[NormalizeModelOwnerValue(value)]
	return row, ok
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
		`INSERT INTO model_owner_presets (value, label, description, enabled, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(value) DO UPDATE SET
		   label = excluded.label,
		   description = excluded.description,
		   enabled = excluded.enabled,
		   updated_at = excluded.updated_at`,
		row.Value,
		row.Label,
		row.Description,
		boolToInt(row.Enabled),
		row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert owner preset: %w", err)
	}
	reloadModelOwnerPresetCache(s.db)
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

	if _, err := tx.Exec("DELETE FROM model_owner_presets"); err != nil {
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
			`INSERT INTO model_owner_presets (value, label, description, enabled, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
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
	reloadModelOwnerPresetCache(s.db)
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

func ensureModelConfigSchema(db *sql.DB) {
	if db == nil {
		return
	}
	if !sqliteColumnExists(db, "model_configs", "input_modalities") {
		if _, err := db.Exec("ALTER TABLE model_configs ADD COLUMN input_modalities TEXT NOT NULL DEFAULT ''"); err != nil {
			log.Warnf("sqlite/modelconfig: add model config input_modalities column: %v", err)
		}
	}
	if !sqliteColumnExists(db, "model_configs", "output_modalities") {
		if _, err := db.Exec("ALTER TABLE model_configs ADD COLUMN output_modalities TEXT NOT NULL DEFAULT ''"); err != nil {
			log.Warnf("sqlite/modelconfig: add model config output_modalities column: %v", err)
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

func defaultModelConfigRows() []ModelConfigRow {
	channels := []string{
		"claude",
		"gemini",
		"vertex",
		"gemini-cli",
		"aistudio",
		"codex",
		"qwen",
		"iflow",
		"kimi",
		"cline",
		"opencode-go",
		"antigravity",
	}

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
				ModelID:     modelID,
				OwnedBy:     ownedBy,
				Description: description,
				Enabled:     true,
				PricingMode: "token",
				Source:      "seed",
			}
			if modelID == "gpt-image-2" {
				row.Description = "Image generation model billed per invocation"
				row.InputModalities = []string{"text"}
				row.OutputModalities = []string{"image"}
				row.PricingMode = "call"
				row.PricePerCall = 0.04
			}
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
			 (model_id, owned_by, description, enabled, input_modalities, output_modalities, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
			`INSERT OR IGNORE INTO model_owner_presets (value, label, description, enabled, updated_at)
			 VALUES (?, ?, '', 1, ?)`,
			value,
			label,
			now,
		)
		if err != nil {
			log.Warnf("sqlite/modelconfig: seed owner preset %s: %v", value, err)
		}
	}

	rows, err := db.Query("SELECT DISTINCT owned_by FROM model_configs WHERE owned_by != ''")
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
			`INSERT OR IGNORE INTO model_owner_presets (value, label, description, enabled, updated_at)
			 VALUES (?, ?, '', 1, ?)`,
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
			 (model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
			 VALUES (?, '', '', 1, 'token', ?, ?, ?, 0, 'legacy-pricing', ?)
			 ON CONFLICT(model_id) DO UPDATE SET
			   pricing_mode = 'token',
			   input_price_per_million = excluded.input_price_per_million,
			   output_price_per_million = excluded.output_price_per_million,
			   cached_price_per_million = excluded.cached_price_per_million,
			   updated_at = excluded.updated_at`,
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
			 WHERE model_id = ?
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

func reloadModelConfigCache(db *sql.DB) {
	rows, err := db.Query(
		`SELECT model_id, owned_by, description, enabled, input_modalities, output_modalities, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, price_per_call, source, updated_at
		 FROM model_configs`,
	)
	if err != nil {
		log.Errorf("sqlite/modelconfig: load model config cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelConfigRow)
	for rows.Next() {
		var row ModelConfigRow
		var enabled int
		var inputModalities string
		var outputModalities string
		if err := rows.Scan(
			&row.ModelID,
			&row.OwnedBy,
			&row.Description,
			&enabled,
			&inputModalities,
			&outputModalities,
			&row.PricingMode,
			&row.InputPricePerMillion,
			&row.OutputPricePerMillion,
			&row.CachedPricePerMillion,
			&row.CacheReadPricePerMillion,
			&row.CacheWritePricePerMillion,
			&row.PricePerCall,
			&row.Source,
			&row.UpdatedAt,
		); err != nil {
			log.Errorf("sqlite/modelconfig: scan model config row: %v", err)
			continue
		}
		row.Enabled = intToBool(enabled)
		row.InputModalities = decodeModelModalities(inputModalities)
		row.OutputModalities = decodeModelModalities(outputModalities)
		row.PricingMode = NormalizePricingMode(row.PricingMode)
		cache[row.ModelID] = row
	}

	modelConfigCacheMu.Lock()
	modelConfigCache = cache
	modelConfigCacheMu.Unlock()
	log.Infof("sqlite/modelconfig: loaded %d model config entries into cache", len(cache))
}

func reloadModelOwnerPresetCache(db *sql.DB) {
	rows, err := db.Query("SELECT value, label, description, enabled, updated_at FROM model_owner_presets")
	if err != nil {
		log.Errorf("sqlite/modelconfig: load model owner preset cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelOwnerPresetRow)
	for rows.Next() {
		var row ModelOwnerPresetRow
		var enabled int
		if err := rows.Scan(&row.Value, &row.Label, &row.Description, &enabled, &row.UpdatedAt); err != nil {
			log.Errorf("sqlite/modelconfig: scan owner preset row: %v", err)
			continue
		}
		row.Value = NormalizeModelOwnerValue(row.Value)
		row.Enabled = intToBool(enabled)
		cache[row.Value] = row
	}

	modelOwnerPresetCacheMu.Lock()
	modelOwnerPresetCache = cache
	modelOwnerPresetCacheMu.Unlock()
	log.Infof("sqlite/modelconfig: loaded %d model owner presets into cache", len(cache))
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

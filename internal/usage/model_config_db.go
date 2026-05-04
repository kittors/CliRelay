package usage

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

type ModelConfigRow struct {
	ModelID               string  `json:"model_id"`
	OwnedBy               string  `json:"owned_by"`
	Description           string  `json:"description"`
	Enabled               bool    `json:"enabled"`
	PricingMode           string  `json:"pricing_mode"`
	InputPricePerMillion  float64 `json:"input_price_per_million"`
	OutputPricePerMillion float64 `json:"output_price_per_million"`
	CachedPricePerMillion float64 `json:"cached_price_per_million"`
	PricePerCall          float64 `json:"price_per_call"`
	Source                string  `json:"source"`
	UpdatedAt             string  `json:"updated_at"`
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
  model_id                 TEXT PRIMARY KEY,
  owned_by                 TEXT NOT NULL DEFAULT '',
  description              TEXT NOT NULL DEFAULT '',
  enabled                  INTEGER NOT NULL DEFAULT 1,
  pricing_mode             TEXT NOT NULL DEFAULT 'token',
  input_price_per_million  REAL NOT NULL DEFAULT 0,
  output_price_per_million REAL NOT NULL DEFAULT 0,
  cached_price_per_million REAL NOT NULL DEFAULT 0,
  price_per_call           REAL NOT NULL DEFAULT 0,
  source                   TEXT NOT NULL DEFAULT 'user',
  updated_at               DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_model_configs_owned_by ON model_configs(owned_by);

CREATE TABLE IF NOT EXISTS model_owner_presets (
  value       TEXT PRIMARY KEY,
  label       TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  enabled     INTEGER NOT NULL DEFAULT 1,
  updated_at  DATETIME NOT NULL
);

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

func initModelConfigTables(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createModelConfigTablesSQL); err != nil {
		log.Errorf("usage: create model config tables: %v", err)
		return
	}
	ensureOpenRouterModelSyncStateSchema(db)
	seedDefaultModelConfigRows(db)
	mergeLegacyPricingIntoModelConfigs(db)
	reloadModelConfigCache(db)
	reloadModelOwnerPresetCache(db)
}

func normalizeModelOwnerValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), "-"))
}

func normalizePricingMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "call") {
		return "call"
	}
	return "token"
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

			ownedBy := normalizeModelOwnerValue(model.OwnedBy)
			if ownedBy == "" {
				ownedBy = normalizeModelOwnerValue(model.Type)
			}
			if ownedBy == "" {
				ownedBy = normalizeModelOwnerValue(channel)
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
			 (model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			row.ModelID,
			row.OwnedBy,
			row.Description,
			boolToInt(row.Enabled),
			normalizePricingMode(row.PricingMode),
			row.InputPricePerMillion,
			row.OutputPricePerMillion,
			row.CachedPricePerMillion,
			row.PricePerCall,
			row.Source,
			now,
		)
		if err != nil {
			log.Warnf("usage: seed model config %s: %v", row.ModelID, err)
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
			log.Warnf("usage: seed owner preset %s: %v", value, err)
		}
	}

	rows, err := db.Query("SELECT DISTINCT owned_by FROM model_configs WHERE owned_by != ''")
	if err != nil {
		log.Warnf("usage: seed owner presets from model configs: %v", err)
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
		value := normalizeModelOwnerValue(owner)
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

func reloadModelConfigCache(db *sql.DB) {
	rows, err := db.Query(
		`SELECT model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at
		 FROM model_configs`,
	)
	if err != nil {
		log.Errorf("usage: load model config cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelConfigRow)
	for rows.Next() {
		var row ModelConfigRow
		var enabled int
		if err := rows.Scan(
			&row.ModelID,
			&row.OwnedBy,
			&row.Description,
			&enabled,
			&row.PricingMode,
			&row.InputPricePerMillion,
			&row.OutputPricePerMillion,
			&row.CachedPricePerMillion,
			&row.PricePerCall,
			&row.Source,
			&row.UpdatedAt,
		); err != nil {
			log.Errorf("usage: scan model config row: %v", err)
			continue
		}
		row.Enabled = intToBool(enabled)
		row.PricingMode = normalizePricingMode(row.PricingMode)
		cache[row.ModelID] = row
	}

	modelConfigCacheMu.Lock()
	modelConfigCache = cache
	modelConfigCacheMu.Unlock()
	log.Infof("usage: loaded %d model config entries into cache", len(cache))
}

func reloadModelOwnerPresetCache(db *sql.DB) {
	rows, err := db.Query("SELECT value, label, description, enabled, updated_at FROM model_owner_presets")
	if err != nil {
		log.Errorf("usage: load model owner preset cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelOwnerPresetRow)
	for rows.Next() {
		var row ModelOwnerPresetRow
		var enabled int
		if err := rows.Scan(&row.Value, &row.Label, &row.Description, &enabled, &row.UpdatedAt); err != nil {
			log.Errorf("usage: scan owner preset row: %v", err)
			continue
		}
		row.Value = normalizeModelOwnerValue(row.Value)
		row.Enabled = intToBool(enabled)
		cache[row.Value] = row
	}

	modelOwnerPresetCacheMu.Lock()
	modelOwnerPresetCache = cache
	modelOwnerPresetCacheMu.Unlock()
	log.Infof("usage: loaded %d model owner presets into cache", len(cache))
}

func upsertLegacyPricingIntoModelConfig(db *sql.DB, modelID string, input, output, cached float64, updatedAt string) {
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
		log.Warnf("usage: sync legacy pricing into model config %s: %v", modelID, err)
		return
	}
	reloadModelConfigCache(db)
}

func ListModelConfigs() []ModelConfigRow {
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

func GetModelConfig(modelID string) (ModelConfigRow, bool) {
	modelConfigCacheMu.RLock()
	defer modelConfigCacheMu.RUnlock()
	row, ok := modelConfigCache[strings.TrimSpace(modelID)]
	return row, ok
}

func UpsertModelConfig(row ModelConfigRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	row.ModelID = strings.TrimSpace(row.ModelID)
	if row.ModelID == "" {
		return fmt.Errorf("usage: model id is required")
	}
	row.OwnedBy = normalizeModelOwnerValue(row.OwnedBy)
	row.PricingMode = normalizePricingMode(row.PricingMode)
	if row.Source == "" {
		row.Source = "user"
	}
	row.UpdatedAt = nowRFC3339()
	_, err := db.Exec(
		`INSERT INTO model_configs
		 (model_id, owned_by, description, enabled, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET
		   owned_by = excluded.owned_by,
		   description = excluded.description,
		   enabled = excluded.enabled,
		   pricing_mode = excluded.pricing_mode,
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   price_per_call = excluded.price_per_call,
		   source = excluded.source,
		   updated_at = excluded.updated_at`,
		row.ModelID,
		row.OwnedBy,
		row.Description,
		boolToInt(row.Enabled),
		row.PricingMode,
		row.InputPricePerMillion,
		row.OutputPricePerMillion,
		row.CachedPricePerMillion,
		row.PricePerCall,
		row.Source,
		row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("usage: upsert model config: %w", err)
	}
	if row.PricingMode == "token" {
		if err := UpsertModelPricing(row.ModelID, row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion); err != nil {
			return err
		}
	} else if err := DeleteModelPricing(row.ModelID); err != nil {
		return err
	}
	if row.OwnedBy != "" {
		if err := UpsertModelOwnerPreset(ModelOwnerPresetRow{
			Value:   row.OwnedBy,
			Label:   ownerLabelForValue(row.OwnedBy),
			Enabled: true,
		}); err != nil {
			return err
		}
	}
	reloadModelConfigCache(db)
	return nil
}

func DeleteModelConfig(modelID string) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return fmt.Errorf("usage: model id is required")
	}
	if _, err := db.Exec("DELETE FROM model_configs WHERE model_id = ?", modelID); err != nil {
		return fmt.Errorf("usage: delete model config: %w", err)
	}
	if err := DeleteModelPricing(modelID); err != nil {
		return err
	}
	reloadModelConfigCache(db)
	return nil
}

func ownerLabelForValue(value string) string {
	value = normalizeModelOwnerValue(value)
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

func ListModelOwnerPresets() []ModelOwnerPresetRow {
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

func GetModelOwnerPreset(value string) (ModelOwnerPresetRow, bool) {
	modelOwnerPresetCacheMu.RLock()
	defer modelOwnerPresetCacheMu.RUnlock()
	row, ok := modelOwnerPresetCache[normalizeModelOwnerValue(value)]
	return row, ok
}

func UpsertModelOwnerPreset(row ModelOwnerPresetRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	row.Value = normalizeModelOwnerValue(row.Value)
	if row.Value == "" {
		return fmt.Errorf("usage: owner value is required")
	}
	if strings.TrimSpace(row.Label) == "" {
		row.Label = ownerLabelForValue(row.Value)
	}
	row.UpdatedAt = nowRFC3339()
	_, err := db.Exec(
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
		return fmt.Errorf("usage: upsert owner preset: %w", err)
	}
	reloadModelOwnerPresetCache(db)
	return nil
}

func ReplaceModelOwnerPresets(rows []ModelOwnerPresetRow) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin owner preset replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM model_owner_presets"); err != nil {
		return fmt.Errorf("usage: clear owner presets: %w", err)
	}
	now := nowRFC3339()
	for _, row := range rows {
		row.Value = normalizeModelOwnerValue(row.Value)
		if row.Value == "" {
			continue
		}
		if strings.TrimSpace(row.Label) == "" {
			row.Label = ownerLabelForValue(row.Value)
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
			return fmt.Errorf("usage: insert owner preset: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit owner preset replace: %w", err)
	}
	reloadModelOwnerPresetCache(db)
	return nil
}

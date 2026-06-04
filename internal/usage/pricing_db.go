package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ModelPricingRow represents a single model's pricing configuration.
type ModelPricingRow struct {
	ModelID                 string  `json:"model_id"`
	InputPricePerMillion    float64 `json:"input_price_per_million"`
	OutputPricePerMillion   float64 `json:"output_price_per_million"`
	CachedPricePerMillion   float64 `json:"cached_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million,omitempty"`
	UpdatedAt               string  `json:"updated_at"`
}

const createPricingTableSQL = `
CREATE TABLE IF NOT EXISTS model_pricing (
  model_id                      TEXT PRIMARY KEY,
  input_price_per_million        REAL NOT NULL DEFAULT 0,
  output_price_per_million       REAL NOT NULL DEFAULT 0,
  cached_price_per_million       REAL NOT NULL DEFAULT 0,
  cache_read_price_per_million   REAL NOT NULL DEFAULT 0,
  cache_write_price_per_million  REAL NOT NULL DEFAULT 0,
  updated_at                    DATETIME NOT NULL
);
`

// In-memory pricing cache for fast cost calculation.
var (
	pricingCache   map[string]ModelPricingRow
	pricingCacheMu sync.RWMutex
)

// initPricingTable creates the model_pricing table and loads the cache.
// Accepts db directly to avoid deadlock when called from InitDB (which holds usageDBMu).
func initPricingTable(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createPricingTableSQL); err != nil {
		log.Errorf("usage: create model_pricing table: %v", err)
		return
	}
	migrateModelPricingCacheColumns(db)
	reloadPricingCache(db)
}

// migrateModelPricingCacheColumns adds cache_read_price_per_million and
// cache_write_price_per_million columns to an existing model_pricing table.
func migrateModelPricingCacheColumns(db *sql.DB) {
	for _, col := range []string{"cache_read_price_per_million", "cache_write_price_per_million"} {
		_, err := db.Exec(fmt.Sprintf("ALTER TABLE model_pricing ADD COLUMN %s REAL NOT NULL DEFAULT 0", col))
		if err != nil {
			if !strings.Contains(err.Error(), "duplicate") {
				log.Warnf("usage: migrate model_pricing column %s: %v", col, err)
			}
		}
	}
}

// reloadPricingCache loads all pricing rows into memory.
// Accepts db directly to avoid deadlock when called from InitDB (which holds usageDBMu).
func reloadPricingCache(db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.Query("SELECT model_id, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, updated_at FROM model_pricing")
	if err != nil {
		log.Errorf("usage: load pricing cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelPricingRow)
	for rows.Next() {
		var row ModelPricingRow
		if err := rows.Scan(&row.ModelID, &row.InputPricePerMillion, &row.OutputPricePerMillion, &row.CachedPricePerMillion, &row.CacheReadPricePerMillion, &row.CacheWritePricePerMillion, &row.UpdatedAt); err != nil {
			log.Errorf("usage: scan pricing row: %v", err)
			continue
		}
		cache[row.ModelID] = row
	}

	pricingCacheMu.Lock()
	pricingCache = cache
	pricingCacheMu.Unlock()
	log.Infof("usage: loaded %d model pricing entries into cache", len(cache))
}

// UpsertModelPricing inserts or updates a model's pricing and refreshes the cache.
// cached is the legacy/compat cache price; cacheRead and cacheWrite are the new semantic fields.
func UpsertModelPricing(modelID string, input, output, cached float64) error {
	return UpsertModelPricingV2(modelID, input, output, cached, 0, 0)
}

// UpsertModelPricingV2 inserts or updates a model's pricing with full cache read/write granularity.
func UpsertModelPricingV2(modelID string, input, output, cached, cacheRead, cacheWrite float64) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO model_pricing (model_id, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   cache_read_price_per_million = excluded.cache_read_price_per_million,
		   cache_write_price_per_million = excluded.cache_write_price_per_million,
		   updated_at = excluded.updated_at`,
		modelID, input, output, cached, cacheRead, cacheWrite, now,
	)
	if err != nil {
		return fmt.Errorf("usage: upsert pricing: %w", err)
	}

	// Update in-memory cache
	pricingCacheMu.Lock()
	if pricingCache == nil {
		pricingCache = make(map[string]ModelPricingRow)
	}
	pricingCache[modelID] = ModelPricingRow{
		ModelID:                 modelID,
		InputPricePerMillion:    input,
		OutputPricePerMillion:   output,
		CachedPricePerMillion:   cached,
		CacheReadPricePerMillion:  cacheRead,
		CacheWritePricePerMillion: cacheWrite,
		UpdatedAt:               now,
	}
	pricingCacheMu.Unlock()
	upsertLegacyPricingIntoModelConfig(db, modelID, input, output, cached, now)
	return nil
}

// GetModelPricing returns the pricing for a single model.
func GetModelPricing(modelID string) (ModelPricingRow, bool) {
	pricingCacheMu.RLock()
	defer pricingCacheMu.RUnlock()
	row, ok := pricingCache[modelID]
	return row, ok
}

// GetAllModelPricing returns all model pricing entries.
func GetAllModelPricing() map[string]ModelPricingRow {
	pricingCacheMu.RLock()
	defer pricingCacheMu.RUnlock()
	result := make(map[string]ModelPricingRow, len(pricingCache))
	for k, v := range pricingCache {
		result[k] = v
	}
	return result
}

// DeleteModelPricing removes a model's pricing.
func DeleteModelPricing(modelID string) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	_, err := db.Exec("DELETE FROM model_pricing WHERE model_id = ?", modelID)
	if err != nil {
		return fmt.Errorf("usage: delete pricing: %w", err)
	}
	pricingCacheMu.Lock()
	delete(pricingCache, modelID)
	pricingCacheMu.Unlock()
	return nil
}

func calculateTokenCost(inputTokens, outputTokens, cachedTokens int64, inputPrice, outputPrice, cachedPrice float64) float64 {
	billableInputTokens := inputTokens
	if cachedTokens > 0 && inputTokens >= cachedTokens {
		// OpenAI-compatible usage reports cached tokens as a subset of input tokens.
		billableInputTokens = inputTokens - cachedTokens
	}
	if cachedPrice <= 0 {
		cachedPrice = inputPrice
	}
	return float64(billableInputTokens)/1_000_000*inputPrice +
		float64(outputTokens)/1_000_000*outputPrice +
		float64(cachedTokens)/1_000_000*cachedPrice
}

// resolveCachePrices returns the effective cache read and cache write prices.
// If explicit cache_read or cache_write prices are set (> 0), they are used.
// Otherwise, the legacy cached_price_per_million is used for both, falling back
// to input_price_per_million if that is also unset.
func resolveCachePrices(cachedPrice, cacheReadPrice, cacheWritePrice, inputPrice float64) (readPrice, writePrice float64) {
	if cacheReadPrice > 0 {
		readPrice = cacheReadPrice
	} else if cachedPrice > 0 {
		readPrice = cachedPrice
	} else {
		readPrice = inputPrice
	}
	if cacheWritePrice > 0 {
		writePrice = cacheWritePrice
	} else if cachedPrice > 0 {
		writePrice = cachedPrice
	} else {
		writePrice = inputPrice
	}
	return
}

// calculateTokenCostV2 is the semantic-driven cost calculator.
// It separates cache read, cache write, and handles the cache_read_included_in_input flag.
func calculateTokenCostV2(inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64, cacheReadIncludedInInput bool, inputPrice, outputPrice, cachedPrice, cacheReadPrice, cacheWritePrice float64) float64 {
	var billableInputTokens int64
	if cacheReadIncludedInInput && cacheReadTokens > 0 && inputTokens >= cacheReadTokens {
		billableInputTokens = inputTokens - cacheReadTokens
	} else {
		billableInputTokens = inputTokens
	}

	readPrice, writePrice := resolveCachePrices(cachedPrice, cacheReadPrice, cacheWritePrice, inputPrice)

	total := float64(billableInputTokens)/1_000_000*inputPrice +
		float64(outputTokens)/1_000_000*outputPrice

	if cacheReadTokens > 0 {
		total += float64(cacheReadTokens) / 1_000_000 * readPrice
	}
	if cacheWriteTokens > 0 {
		total += float64(cacheWriteTokens) / 1_000_000 * writePrice
	}

	return total
}

// resolveModelPricing returns the effective pricing for a model from either
// ModelConfigRow cache or ModelPricingRow cache, along with a boolean indicating
// whether the model is enabled (or found at all).
func resolveModelPricing(modelID string) (inputPrice, outputPrice, cachedPrice, cacheReadPrice, cacheWritePrice float64, enabled bool) {
	modelConfigCacheMu.RLock()
	if row, ok := modelConfigCache[modelID]; ok {
		modelConfigCacheMu.RUnlock()
		enabled = row.Enabled
		return row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion, row.CacheReadPricePerMillion, row.CacheWritePricePerMillion, enabled
	}
	modelConfigCacheMu.RUnlock()

	pricingCacheMu.RLock()
	row, ok := pricingCache[modelID]
	pricingCacheMu.RUnlock()
	if !ok {
		return 0, 0, 0, 0, 0, false
	}
	return row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion, row.CacheReadPricePerMillion, row.CacheWritePricePerMillion, true
}

// CalculateCost computes the cost for a request based on the model's pricing.
// It uses the legacy cached_tokens field and OpenAI-compatible heuristic.
// Returns 0 if no pricing is configured for the model.
func CalculateCost(modelID string, inputTokens, outputTokens, cachedTokens int64) float64 {
	modelConfigCacheMu.RLock()
	if row, ok := modelConfigCache[modelID]; ok {
		modelConfigCacheMu.RUnlock()
		if !row.Enabled {
			return 0
		}
		if normalizePricingMode(row.PricingMode) == "call" {
			return row.PricePerCall
		}
		return calculateTokenCost(inputTokens, outputTokens, cachedTokens, row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion)
	}
	modelConfigCacheMu.RUnlock()

	pricingCacheMu.RLock()
	row, ok := pricingCache[modelID]
	pricingCacheMu.RUnlock()
	if !ok {
		return 0
	}
	return calculateTokenCost(inputTokens, outputTokens, cachedTokens, row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion)
}

// resolveCallPricing checks whether a model uses "call" pricing mode and returns
// the per-call price if so, along with a boolean. This avoids re-checking the
// model config cache directly in CalculateCostV2.
func resolveCallPricing(modelID string) (pricePerCall float64, isCall bool) {
	modelConfigCacheMu.RLock()
	defer modelConfigCacheMu.RUnlock()
	if row, ok := modelConfigCache[modelID]; ok && row.Enabled && normalizePricingMode(row.PricingMode) == "call" {
		return row.PricePerCall, true
	}
	return 0, false
}

// CalculateCostV2 computes the cost using the semantic cache read/write pricing model.
// It uses cache_read_tokens, cache_write_tokens, and cache_read_included_in_input
// from TokenStats for accurate cost calculation.
func CalculateCostV2(modelID string, tokens TokenStats) float64 {
	// Check for per-call pricing first
	if pricePerCall, isCall := resolveCallPricing(modelID); isCall {
		return pricePerCall
	}

	inputPrice, outputPrice, cachedPrice, cacheReadPrice, cacheWritePrice, enabled := resolveModelPricing(modelID)
	if !enabled {
		return 0
	}

	// Use cache read/write tokens from the stats; fall back to legacy CachedTokens
	// for the field that is populated.
	cacheReadTokens := tokens.CacheReadTokens
	cacheWriteTokens := tokens.CacheWriteTokens
	cacheReadIncludedInInput := tokens.CacheReadIncludedInInput

	if cacheReadTokens == 0 && cacheWriteTokens == 0 && tokens.CachedTokens > 0 {
		// Legacy path: no semantic fields populated, use the old heuristic
		return calculateTokenCost(tokens.InputTokens, tokens.OutputTokens, tokens.CachedTokens, inputPrice, outputPrice, cachedPrice)
	}

	return calculateTokenCostV2(
		tokens.InputTokens,
		tokens.OutputTokens,
		cacheReadTokens,
		cacheWriteTokens,
		cacheReadIncludedInInput,
		inputPrice,
		outputPrice,
		cachedPrice,
		cacheReadPrice,
		cacheWritePrice,
	)
}

// QueryTotalCostByKey returns the total accumulated cost for a given API key.
func QueryTotalCostByKey(apiKey string) (float64, error) {
	db := getDB()
	if db == nil {
		return 0, nil
	}
	var total float64
	err := db.QueryRow(
		"SELECT COALESCE(SUM(cost), 0) FROM request_logs WHERE api_key = ?",
		apiKey,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("usage: query total cost: %w", err)
	}
	return total, nil
}

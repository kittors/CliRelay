package usage

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// ModelPricingRow represents a single model's pricing configuration.
type ModelPricingRow struct {
	ModelID               string  `json:"model_id"`
	InputPricePerMillion  float64 `json:"input_price_per_million"`
	OutputPricePerMillion float64 `json:"output_price_per_million"`
	CachedPricePerMillion float64 `json:"cached_price_per_million"`
	UpdatedAt             string  `json:"updated_at"`
}

const createPricingTableSQL = `
CREATE TABLE IF NOT EXISTS model_pricing (
  model_id                TEXT PRIMARY KEY,
  input_price_per_million  REAL NOT NULL DEFAULT 0,
  output_price_per_million REAL NOT NULL DEFAULT 0,
  cached_price_per_million REAL NOT NULL DEFAULT 0,
  updated_at              DATETIME NOT NULL
);
`

// In-memory pricing cache for fast cost calculation.
var (
	pricingCache   map[string]ModelPricingRow
	pricingCacheMu sync.RWMutex
)

// initPricingTable creates the model_pricing table and loads the cache.
func initPricingTable() {
	db := getDB()
	if db == nil {
		return
	}
	if _, err := db.Exec(createPricingTableSQL); err != nil {
		log.Errorf("usage: create model_pricing table: %v", err)
		return
	}
	reloadPricingCache()
}

// reloadPricingCache loads all pricing rows into memory.
func reloadPricingCache() {
	db := getDB()
	if db == nil {
		return
	}
	rows, err := db.Query("SELECT model_id, input_price_per_million, output_price_per_million, cached_price_per_million, updated_at FROM model_pricing")
	if err != nil {
		log.Errorf("usage: load pricing cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]ModelPricingRow)
	for rows.Next() {
		var row ModelPricingRow
		if err := rows.Scan(&row.ModelID, &row.InputPricePerMillion, &row.OutputPricePerMillion, &row.CachedPricePerMillion, &row.UpdatedAt); err != nil {
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
func UpsertModelPricing(modelID string, input, output, cached float64) error {
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(
		`INSERT INTO model_pricing (model_id, input_price_per_million, output_price_per_million, cached_price_per_million, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(model_id) DO UPDATE SET
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   updated_at = excluded.updated_at`,
		modelID, input, output, cached, now,
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
		ModelID:               modelID,
		InputPricePerMillion:  input,
		OutputPricePerMillion: output,
		CachedPricePerMillion: cached,
		UpdatedAt:             now,
	}
	pricingCacheMu.Unlock()
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

// CalculateCost computes the cost for a request based on the model's pricing.
// Returns 0 if no pricing is configured for the model.
func CalculateCost(modelID string, inputTokens, outputTokens, cachedTokens int64) float64 {
	pricingCacheMu.RLock()
	row, ok := pricingCache[modelID]
	pricingCacheMu.RUnlock()
	if !ok {
		return 0
	}
	cost := float64(inputTokens)/1_000_000*row.InputPricePerMillion +
		float64(outputTokens)/1_000_000*row.OutputPricePerMillion +
		float64(cachedTokens)/1_000_000*row.CachedPricePerMillion
	return cost
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

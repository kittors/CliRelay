package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	log "github.com/sirupsen/logrus"
)

// ModelPricingRow represents a single model's pricing configuration.
type ModelPricingRow struct {
	ModelID                   string  `json:"model_id"`
	InputPricePerMillion      float64 `json:"input_price_per_million"`
	OutputPricePerMillion     float64 `json:"output_price_per_million"`
	CachedPricePerMillion     float64 `json:"cached_price_per_million"`
	CacheReadPricePerMillion  float64 `json:"cache_read_price_per_million,omitempty"`
	CacheWritePricePerMillion float64 `json:"cache_write_price_per_million,omitempty"`
	UpdatedAt                 string  `json:"updated_at"`
}

const createPricingTableSQL = `
CREATE TABLE IF NOT EXISTS model_pricing (
  tenant_id                     TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  model_id                      TEXT NOT NULL,
  input_price_per_million        REAL NOT NULL DEFAULT 0,
  output_price_per_million       REAL NOT NULL DEFAULT 0,
  cached_price_per_million       REAL NOT NULL DEFAULT 0,
  cache_read_price_per_million   REAL NOT NULL DEFAULT 0,
  cache_write_price_per_million  REAL NOT NULL DEFAULT 0,
  updated_at                    DATETIME NOT NULL,
  PRIMARY KEY (tenant_id, model_id)
);
`

// In-memory pricing cache for fast cost calculation.
var (
	pricingCache   map[string]map[string]ModelPricingRow
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
	migrateModelPricingTenantSchema(db)
	reloadPricingCache(db)
}

func usageSQLiteCompositeTenantPrimaryKey(db *sql.DB, table string) bool {
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

func migrateModelPricingTenantSchema(db *sql.DB) {
	if db == nil || !sqliteColumnExists(db, "model_pricing", "model_id") || usageSQLiteCompositeTenantPrimaryKey(db, "model_pricing") {
		return
	}
	hasTenant := sqliteColumnExists(db, "model_pricing", "tenant_id")
	tx, err := db.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if !hasTenant {
		if _, err = tx.Exec("ALTER TABLE model_pricing ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001'"); err != nil {
			return
		}
	}
	if _, err = tx.Exec("ALTER TABLE model_pricing RENAME TO model_pricing_legacy"); err != nil {
		return
	}
	if _, err = tx.Exec(createPricingTableSQL); err != nil {
		return
	}
	_, err = tx.Exec("INSERT INTO model_pricing(tenant_id,model_id,input_price_per_million,output_price_per_million,cached_price_per_million,cache_read_price_per_million,cache_write_price_per_million,updated_at) SELECT tenant_id,model_id,input_price_per_million,output_price_per_million,cached_price_per_million,cache_read_price_per_million,cache_write_price_per_million,updated_at FROM model_pricing_legacy")
	if err != nil {
		return
	}
	if _, err = tx.Exec("DROP TABLE model_pricing_legacy"); err != nil {
		return
	}
	_ = tx.Commit()
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
	rows, err := db.Query("SELECT tenant_id, model_id, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, updated_at FROM model_pricing")
	if err != nil {
		log.Errorf("usage: load pricing cache: %v", err)
		return
	}
	defer rows.Close()

	cache := make(map[string]map[string]ModelPricingRow)
	for rows.Next() {
		var tenantID string
		var row ModelPricingRow
		if err := rows.Scan(&tenantID, &row.ModelID, &row.InputPricePerMillion, &row.OutputPricePerMillion, &row.CachedPricePerMillion, &row.CacheReadPricePerMillion, &row.CacheWritePricePerMillion, &row.UpdatedAt); err != nil {
			log.Errorf("usage: scan pricing row: %v", err)
			continue
		}
		tenantID = normalizeTenantID(tenantID)
		if cache[tenantID] == nil {
			cache[tenantID] = make(map[string]ModelPricingRow)
		}
		cache[tenantID][row.ModelID] = row
	}

	pricingCacheMu.Lock()
	pricingCache = cache
	pricingCacheMu.Unlock()
	log.Infof("usage: loaded %d model pricing entries into cache", len(cache))
}

// UpsertModelPricing inserts or updates a model's pricing and refreshes the cache.
// cached is the legacy/compat cache price; cacheRead and cacheWrite are the new semantic fields.
func UpsertModelPricing(modelID string, input, output, cached float64) error {
	return UpsertModelPricingForTenant(systemTenantID, modelID, input, output, cached)
}

func UpsertModelPricingForTenant(tenantID, modelID string, input, output, cached float64) error {
	return UpsertModelPricingV2ForTenant(tenantID, modelID, input, output, cached, 0, 0)
}

// UpsertModelPricingV2 inserts or updates a model's pricing with full cache read/write granularity.
func UpsertModelPricingV2(modelID string, input, output, cached, cacheRead, cacheWrite float64) error {
	return UpsertModelPricingV2ForTenant(systemTenantID, modelID, input, output, cached, cacheRead, cacheWrite)
}

func UpsertModelPricingV2ForTenant(tenantID, modelID string, input, output, cached, cacheRead, cacheWrite float64) error {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := configsync.Exec(context.Background(), db, []cluster.ConfigEvent{configsync.Event(configsync.DomainPricing, tenantID)},
		`INSERT INTO model_pricing (tenant_id, model_id, input_price_per_million, output_price_per_million, cached_price_per_million, cache_read_price_per_million, cache_write_price_per_million, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, model_id) DO UPDATE SET
		   input_price_per_million = excluded.input_price_per_million,
		   output_price_per_million = excluded.output_price_per_million,
		   cached_price_per_million = excluded.cached_price_per_million,
		   cache_read_price_per_million = excluded.cache_read_price_per_million,
		   cache_write_price_per_million = excluded.cache_write_price_per_million,
		   updated_at = excluded.updated_at`,
		tenantID, modelID, input, output, cached, cacheRead, cacheWrite, now,
	)
	if err != nil {
		return fmt.Errorf("usage: upsert pricing: %w", err)
	}

	// Update in-memory cache
	pricingCacheMu.Lock()
	if pricingCache == nil {
		pricingCache = make(map[string]map[string]ModelPricingRow)
	}
	if pricingCache[tenantID] == nil {
		pricingCache[tenantID] = make(map[string]ModelPricingRow)
	}
	pricingCache[tenantID][modelID] = ModelPricingRow{
		ModelID:                   modelID,
		InputPricePerMillion:      input,
		OutputPricePerMillion:     output,
		CachedPricePerMillion:     cached,
		CacheReadPricePerMillion:  cacheRead,
		CacheWritePricePerMillion: cacheWrite,
		UpdatedAt:                 now,
	}
	pricingCacheMu.Unlock()
	upsertLegacyPricingIntoModelConfigForTenant(db, tenantID, modelID, input, output, cached, now)
	return nil
}

// GetModelPricing returns the pricing for a single model.
func GetModelPricing(modelID string) (ModelPricingRow, bool) {
	return GetModelPricingForTenant(systemTenantID, modelID)
}

// GetModelPricingForTenant returns tenant pricing, falling back to the system catalog when absent.
// OpenRouter sync writes prices to the system tenant; business tenants inherit them unless overridden.
func GetModelPricingForTenant(tenantID, modelID string) (ModelPricingRow, bool) {
	tenantID = normalizeTenantID(tenantID)
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ModelPricingRow{}, false
	}
	pricingCacheMu.RLock()
	defer pricingCacheMu.RUnlock()
	if row, ok := pricingCache[tenantID][modelID]; ok {
		return row, true
	}
	if isSystemTenant(tenantID) {
		return ModelPricingRow{}, false
	}
	row, ok := pricingCache[systemTenantID][modelID]
	return row, ok
}

// GetAllModelPricing returns all model pricing entries.
func GetAllModelPricing() map[string]ModelPricingRow {
	return GetAllModelPricingForTenant(systemTenantID)
}

// GetAllModelPricingForTenant returns tenant pricing merged with system-catalog inheritance.
// Tenant-owned rows override system rows for the same model_id.
func GetAllModelPricingForTenant(tenantID string) map[string]ModelPricingRow {
	tenantID = normalizeTenantID(tenantID)
	pricingCacheMu.RLock()
	defer pricingCacheMu.RUnlock()

	tenantCache := pricingCache[tenantID]
	if isSystemTenant(tenantID) {
		result := make(map[string]ModelPricingRow, len(tenantCache))
		for k, v := range tenantCache {
			result[k] = v
		}
		return result
	}

	systemCache := pricingCache[systemTenantID]
	result := make(map[string]ModelPricingRow, len(systemCache)+len(tenantCache))
	for k, v := range systemCache {
		result[k] = v
	}
	for k, v := range tenantCache {
		result[k] = v
	}
	return result
}

// DeleteModelPricing removes a model's pricing.
func DeleteModelPricing(modelID string) error {
	return DeleteModelPricingForTenant(systemTenantID, modelID)
}
func DeleteModelPricingForTenant(tenantID, modelID string) error {
	tenantID = normalizeTenantID(tenantID)
	db := getDB()
	if db == nil {
		return fmt.Errorf("usage: database not initialised")
	}
	_, err := configsync.Exec(context.Background(), db, []cluster.ConfigEvent{configsync.Event(configsync.DomainPricing, tenantID)}, "DELETE FROM model_pricing WHERE tenant_id = ? AND model_id = ?", tenantID, modelID)
	if err != nil {
		return fmt.Errorf("usage: delete pricing: %w", err)
	}
	pricingCacheMu.Lock()
	delete(pricingCache[tenantID], modelID)
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

func modelPricingLookupModelIDs(modelID string) []string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	lookupIDs := []string{modelID}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == modelID {
			return
		}
		for _, existing := range lookupIDs {
			if existing == candidate {
				return
			}
		}
		lookupIDs = append(lookupIDs, candidate)
	}

	if prefix, _, found := strings.Cut(modelID, "/"); found {
		if strings.TrimSpace(prefix) != "" {
			add(openRouterProviderlessModelID(modelID))
		}
	}
	if variantSeparator := strings.LastIndex(modelID, ":"); variantSeparator > 0 {
		add(modelID[:variantSeparator])
	}
	if !strings.Contains(modelID, "/") {
		prefix, suffix, found := strings.Cut(modelID, "-")
		if found && strings.TrimSpace(prefix) != "" {
			add(suffix)
		}
	}
	// Strip the last dash-segment from the providerless ID so models with
	// variant/tier suffixes like "-thinking", "-agent", "-high", "-low",
	// "-medium", "-extra-low", "-tiered" can inherit pricing from the base model.
	providerless := modelID
	if idx := strings.Index(modelID, "/"); idx > 0 {
		providerless = modelID[idx+1:]
	}
	if lastDash := strings.LastIndex(providerless, "-"); lastDash > 0 {
		add(providerless[:lastDash])
	}
	return lookupIDs
}

func hasEffectiveModelPricing(row ModelConfigRow) bool {
	if normalizePricingMode(row.PricingMode) == "call" {
		return row.PricePerCall > 0
	}
	return row.InputPricePerMillion > 0 ||
		row.OutputPricePerMillion > 0 ||
		row.CachedPricePerMillion > 0 ||
		row.CacheReadPricePerMillion > 0 ||
		row.CacheWritePricePerMillion > 0
}

func modelConfigRowFromLegacyPricing(pricing ModelPricingRow) ModelConfigRow {
	return ModelConfigRow{
		ModelID:                   pricing.ModelID,
		Enabled:                   true,
		PricingMode:               "token",
		InputPricePerMillion:      pricing.InputPricePerMillion,
		OutputPricePerMillion:     pricing.OutputPricePerMillion,
		CachedPricePerMillion:     pricing.CachedPricePerMillion,
		CacheReadPricePerMillion:  pricing.CacheReadPricePerMillion,
		CacheWritePricePerMillion: pricing.CacheWritePricePerMillion,
	}
}

func resolveModelPricingRow(modelID string, lookup func(string) (ModelConfigRow, bool)) (ModelConfigRow, bool) {
	var firstFound ModelConfigRow
	foundAny := false
	exactEnabled := true
	exactExists := false
	for i, lookupID := range modelPricingLookupModelIDs(modelID) {
		row, ok := lookup(lookupID)
		if !ok {
			continue
		}
		if !foundAny {
			firstFound = row
			foundAny = true
		}
		if i == 0 {
			exactEnabled = row.Enabled
			exactExists = true
		}
		if hasEffectiveModelPricing(row) {
			if exactExists && i > 0 {
				row.Enabled = row.Enabled && exactEnabled
			}
			return row, true
		}
	}
	return firstFound, foundAny
}

// ResolveModelPricingRow resolves exact pricing first, then inherited base
// candidates for provider-prefixed, tagged, or dash-prefixed runtime model IDs.
// Config rows take precedence over legacy pricing for each candidate; unpriced
// rows do not block a later priced base candidate. Map keys must be normalized
// to lowercase IDs.
func ResolveModelPricingRow(modelID string, configByID map[string]ModelConfigRow, pricingByID map[string]ModelPricingRow) (ModelConfigRow, bool) {
	return resolveModelPricingRow(modelID, func(lookupID string) (ModelConfigRow, bool) {
		key := strings.ToLower(strings.TrimSpace(lookupID))
		if row, ok := configByID[key]; ok {
			return row, true
		}
		if pricing, ok := pricingByID[key]; ok {
			return modelConfigRowFromLegacyPricing(pricing), true
		}
		return ModelConfigRow{}, false
	})
}

// resolveModelPricingRowForTenant preserves tenant-over-system inheritance and
// ModelConfigRow-over-legacy-pricing precedence for every lookup candidate.
func resolveModelPricingRowForTenant(tenantID, modelID string) (ModelConfigRow, bool) {
	return resolveModelPricingRow(modelID, func(lookupID string) (ModelConfigRow, bool) {
		if row, ok := GetModelConfigForTenant(tenantID, lookupID); ok {
			return row, true
		}
		if pricing, ok := GetModelPricingForTenant(tenantID, lookupID); ok {
			return modelConfigRowFromLegacyPricing(pricing), true
		}
		return ModelConfigRow{}, false
	})
}

// resolveModelPricing returns the effective pricing for a model from either
// ModelConfigRow cache or ModelPricingRow cache, along with a boolean indicating
// whether the model is enabled (or found at all).
// Both lookups inherit system-tenant catalog prices when the business tenant has no override.
func resolveModelPricingForTenant(tenantID, modelID string) (inputPrice, outputPrice, cachedPrice, cacheReadPrice, cacheWritePrice float64, enabled bool) {
	row, ok := resolveModelPricingRowForTenant(tenantID, modelID)
	if !ok {
		return 0, 0, 0, 0, 0, false
	}
	return row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion, row.CacheReadPricePerMillion, row.CacheWritePricePerMillion, row.Enabled
}

// CalculateCost computes the cost for a request based on the model's pricing.
// It uses the legacy cached_tokens field and OpenAI-compatible heuristic.
// Returns 0 if no pricing is configured for the model.
func CalculateCost(modelID string, inputTokens, outputTokens, cachedTokens int64) float64 {
	return CalculateCostForTenant(systemTenantID, modelID, inputTokens, outputTokens, cachedTokens)
}
func CalculateCostForTenant(tenantID, modelID string, inputTokens, outputTokens, cachedTokens int64) float64 {
	row, ok := resolveModelPricingRowForTenant(tenantID, modelID)
	if !ok || !row.Enabled {
		return 0
	}
	if normalizePricingMode(row.PricingMode) == "call" {
		return row.PricePerCall
	}
	return calculateTokenCost(inputTokens, outputTokens, cachedTokens, row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion)
}

// resolveCallPricing checks whether a model uses "call" pricing mode and returns
// the per-call price if so, along with a boolean. This avoids re-checking the
// model config cache directly in CalculateCostV2.
func resolveCallPricingForTenant(tenantID, modelID string) (pricePerCall float64, isCall bool) {
	if row, ok := resolveModelPricingRowForTenant(tenantID, modelID); ok && row.Enabled && normalizePricingMode(row.PricingMode) == "call" {
		return row.PricePerCall, true
	}
	return 0, false
}

// CalculateCostV2 computes the cost using the semantic cache read/write pricing model.
// It uses cache_read_tokens, cache_write_tokens, and cache_read_included_in_input
// from TokenStats for accurate cost calculation.
func CalculateCostV2(modelID string, tokens TokenStats) float64 {
	return CalculateCostV2ForTenant(systemTenantID, modelID, tokens)
}
func CalculateCostV2ForTenant(tenantID, modelID string, tokens TokenStats) float64 {
	// Check for per-call pricing first
	if pricePerCall, isCall := resolveCallPricingForTenant(tenantID, modelID); isCall {
		return pricePerCall
	}

	inputPrice, outputPrice, cachedPrice, cacheReadPrice, cacheWritePrice, enabled := resolveModelPricingForTenant(tenantID, modelID)
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
// Reads lifetime usage rollup so detail retention cleanup cannot shrink lifetime limits.
func QueryTotalCostByKey(apiKey string) (float64, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return 0, nil
	}
	tenantID := ResolveAPIKeyTenant(apiKey)
	if tenantID == "" {
		tenantID = systemTenantID
	}
	apiKeyID := ""
	if identity := ResolveAPIKeyIdentity(apiKey); identity != nil {
		apiKeyID = identity.ID
	}
	if apiKeyID == "" {
		if row := GetAPIKey(apiKey); row != nil {
			apiKeyID = strings.TrimSpace(row.ID)
		}
	}
	if apiKeyID == "" {
		return 0, nil
	}
	agg, err := queryRollupAgg(rollupFilter{
		TenantID:   tenantID,
		BucketKind: rollupBucketLifetime,
		APIKeyIDs:  []string{apiKeyID},
	})
	if err != nil {
		return 0, fmt.Errorf("usage: query total cost: %w", err)
	}
	return agg.CostTotal, nil
}

// QueryTodayCostByKey is defined in api_key_daily_spending_reset.go and returns
// effective project-day cost (raw sum minus same-day reset baseline when present).

// ReloadPricingCache re-reads every pricing row into the in-memory cache. A
// cluster node calls it when another node changed prices; its own writes
// update the cache as they go.
func ReloadPricingCache() {
	reloadPricingCache(getDB())
}

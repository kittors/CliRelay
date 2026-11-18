package usage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

// Models the relay serves appear on their own: discovery registers what the
// upstreams list, and a request for a new model is logged before anyone has
// priced it. Until a price exists such requests are stored at zero, and a
// business tenant without its own catalog inherits the system catalog, which
// had no price either. The autofill gives every model in use, and every model
// the registry offers, the price and the descriptive fields OpenRouter lists for
// it, where the catalog has none.
//
// It only ever fills a gap:
//   - a row an operator entered (source "user") is never touched;
//   - prices are written only into a row without an effective price, and the
//     other fields only where they are empty;
//   - a system-catalog row is only added or filled when no tenant's current
//     effective price for the model changes (a tenant whose own base row prices
//     the model keeps that price);
//   - a model whose OpenRouter listings disagree on price is left alone.
// Every price it writes queues a fill job, so the requests already logged at
// zero get their cost.

const (
	modelCatalogAutofillInterval = 30 * time.Minute
	// The OpenRouter list is large and changes slowly; one fetch serves the
	// passes of several hours.
	modelCatalogAutofillListTTL   = 6 * time.Hour
	modelCatalogAutofillFetchTime = 60 * time.Second
	modelCatalogAutofillSource    = openRouterModelSource
)

var modelCatalogAutofillList struct {
	sync.Mutex
	models    []OpenRouterRemoteModel
	fetchedAt time.Time
}

func modelCatalogAutofillRemoteModels(ctx context.Context, now time.Time) ([]OpenRouterRemoteModel, error) {
	modelCatalogAutofillList.Lock()
	defer modelCatalogAutofillList.Unlock()
	if len(modelCatalogAutofillList.models) > 0 && now.Sub(modelCatalogAutofillList.fetchedAt) < modelCatalogAutofillListTTL {
		return modelCatalogAutofillList.models, nil
	}
	openRouterModelFetcherMu.RLock()
	fetcher := openRouterModelFetcher
	openRouterModelFetcherMu.RUnlock()
	if fetcher == nil {
		fetcher = fetchOpenRouterModels
	}
	fetchCtx, cancel := context.WithTimeout(ctx, modelCatalogAutofillFetchTime)
	defer cancel()
	models, err := fetcher(fetchCtx)
	if err != nil {
		return nil, err
	}
	modelCatalogAutofillList.models = models
	modelCatalogAutofillList.fetchedAt = now
	return models, nil
}

func resetModelCatalogAutofillListForTest() {
	modelCatalogAutofillList.Lock()
	defer modelCatalogAutofillList.Unlock()
	modelCatalogAutofillList.models = nil
	modelCatalogAutofillList.fetchedAt = time.Time{}
}

type autofillTarget struct {
	tenantID string
	modelID  string
}

// modelCatalogAutofillTargets lists the tenant and model pairs without an
// effective price: models that served requests in the last days, per tenant,
// and the models the registry serves, for the system catalog.
func modelCatalogAutofillTargets(now time.Time) ([]autofillTarget, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	seen := map[string]struct{}{}
	targets := make([]autofillTarget, 0)
	add := func(tenantID, modelID string) {
		tenantID = normalizeTenantID(tenantID)
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		key := tenantID + "\x00" + strings.ToLower(modelID)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		if row, ok := resolveModelPricingRowForTenant(tenantID, modelID); ok && (!row.Enabled || hasEffectiveModelPricing(row)) {
			// Priced, or switched off on purpose: nothing to fill.
			return
		}
		targets = append(targets, autofillTarget{tenantID: tenantID, modelID: modelID})
	}
	since := localDayKeyAtLocation(now.Add(-pricingResyncRetention-24*time.Hour), metadataForUsageDB(db).loc)
	// Only models that answered at least once: a name a client asked for that no
	// upstream serves must not get a catalog row of its own.
	rows, err := db.Query(`SELECT DISTINCT tenant_id, model FROM usage_rollup_buckets
		WHERE bucket_kind = ? AND bucket_start >= ? AND success_count > 0`, rollupBucketDay, since)
	if err != nil {
		return nil, fmt.Errorf("usage: autofill models in use: %w", err)
	}
	for rows.Next() {
		var tenantID, modelID string
		if err := rows.Scan(&tenantID, &modelID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		add(tenantID, modelID)
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	if reg := registry.GetGlobalRegistry(); reg != nil {
		for _, model := range reg.GetAvailableModels("openai") {
			if id, _ := model["id"].(string); id != "" {
				add(systemTenantID, id)
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].tenantID != targets[j].tenantID {
			return targets[i].tenantID < targets[j].tenantID
		}
		return targets[i].modelID < targets[j].modelID
	})
	return targets, nil
}

// autofillRemoteIndex maps local model IDs (lowercase) to the OpenRouter
// listings that resolve to them, the way the OpenRouter sync names its rows.
func autofillRemoteIndex(models []OpenRouterRemoteModel) map[string][]OpenRouterRemoteModel {
	index := make(map[string][]OpenRouterRemoteModel)
	for _, model := range models {
		remoteID := strings.TrimSpace(model.ID)
		if remoteID == "" {
			continue
		}
		owner := openRouterOwnerFromModelID(remoteID)
		local := strings.ToLower(strings.TrimSpace(openRouterLocalModelID(remoteID, owner)))
		if local == "" {
			continue
		}
		index[local] = append(index[local], model)
	}
	return index
}

// autofillRemoteFor picks the listing to fill a model from. Listings of the
// same model by several providers usually share a price; when they do not, no
// choice is safe and the model is skipped.
//
// A listing that yields no price is skipped too. Video and music generators
// (x-ai/grok-imagine-video-1.5, google/veo-3.1, google/lyria-3-pro-preview)
// list zero prompt and completion prices, and an image model billed per call
// stays at zero without a registry default (Grok Imagine is billed against the
// subscription). Filling from such a listing wrote a row that was still
// unpriced, so every run rewrote it and queued an empty fill job. autofillMerge
// prices the row through this same blank-row computation, so a row it writes
// is priced.
func autofillRemoteFor(index map[string][]OpenRouterRemoteModel, modelID string) (OpenRouterRemoteModel, bool) {
	listings := index[strings.ToLower(strings.TrimSpace(modelID))]
	if len(listings) == 0 {
		return OpenRouterRemoteModel{}, false
	}
	first := autofillPricedRow(ModelConfigRow{ModelID: modelID}, listings[0])
	if !hasEffectiveModelPricing(first) {
		return OpenRouterRemoteModel{}, false
	}
	for _, other := range listings[1:] {
		if !samePricing(first, autofillPricedRow(ModelConfigRow{ModelID: modelID}, other)) {
			return OpenRouterRemoteModel{}, false
		}
	}
	return listings[0], true
}

// autofillPricedRow is row as the OpenRouter sync would write it from model.
func autofillPricedRow(row ModelConfigRow, model OpenRouterRemoteModel) ModelConfigRow {
	owner := openRouterOwnerFromModelID(strings.TrimSpace(model.ID))
	openRouterApplyModelSync(&row, model, owner)
	return row
}

func samePricing(a, b ModelConfigRow) bool {
	return normalizePricingMode(a.PricingMode) == normalizePricingMode(b.PricingMode) &&
		a.InputPricePerMillion == b.InputPricePerMillion &&
		a.OutputPricePerMillion == b.OutputPricePerMillion &&
		a.CachedPricePerMillion == b.CachedPricePerMillion &&
		a.CacheReadPricePerMillion == b.CacheReadPricePerMillion &&
		a.CacheWritePricePerMillion == b.CacheWritePricePerMillion &&
		a.PricePerCall == b.PricePerCall &&
		a.Enabled == b.Enabled
}

// autofillMerge fills existing from the OpenRouter values: the price as a whole
// (existing has none), and every other field only where existing is empty.
func autofillMerge(existing ModelConfigRow, model OpenRouterRemoteModel) ModelConfigRow {
	filled := autofillPricedRow(ModelConfigRow{ModelID: existing.ModelID, Enabled: true}, model)
	out := existing
	out.PricingMode = filled.PricingMode
	out.InputPricePerMillion = filled.InputPricePerMillion
	out.OutputPricePerMillion = filled.OutputPricePerMillion
	out.CachedPricePerMillion = filled.CachedPricePerMillion
	out.CacheReadPricePerMillion = filled.CacheReadPricePerMillion
	out.CacheWritePricePerMillion = filled.CacheWritePricePerMillion
	out.PricePerCall = filled.PricePerCall
	if strings.TrimSpace(out.OwnedBy) == "" {
		out.OwnedBy = filled.OwnedBy
	}
	if strings.TrimSpace(out.DisplayName) == "" {
		out.DisplayName = filled.DisplayName
	}
	if strings.TrimSpace(out.Description) == "" {
		out.Description = filled.Description
	}
	if len(out.InputModalities) == 0 {
		out.InputModalities = filled.InputModalities
	}
	if len(out.OutputModalities) == 0 {
		out.OutputModalities = filled.OutputModalities
	}
	if out.ContextLength == 0 {
		out.ContextLength = filled.ContextLength
	}
	if out.MaxCompletionTokens == 0 {
		out.MaxCompletionTokens = filled.MaxCompletionTokens
	}
	if len(out.SupportedParameters) == 0 {
		out.SupportedParameters = filled.SupportedParameters
	}
	if strings.TrimSpace(out.Reasoning) == "" {
		out.Reasoning = filled.Reasoning
	}
	if strings.TrimSpace(out.KnowledgeCutoff) == "" {
		out.KnowledgeCutoff = filled.KnowledgeCutoff
	}
	out.Source = modelCatalogAutofillSource
	return out
}

// effectivePricing is the resolved price a tenant would bill a model with.
func effectivePricing(row ModelConfigRow, ok bool) (ModelConfigRow, bool) {
	if !ok || !row.Enabled || !hasEffectiveModelPricing(row) {
		return ModelConfigRow{}, false
	}
	return row, true
}

// systemFillKeepsTenantPrices checks that adding row to the system catalog does
// not change the price any tenant bills the model with today, unless that
// tenant had none. Business tenants resolve through their own rows first and
// then the system catalog; a tenant whose own base row prices the model would
// otherwise switch to the new system row.
func systemFillKeepsTenantPrices(tenantIDs []string, row ModelConfigRow) bool {
	modelKey := strings.ToLower(strings.TrimSpace(row.ModelID))
	for _, tenantID := range tenantIDs {
		tenantID = normalizeTenantID(tenantID)
		if isSystemTenant(tenantID) {
			continue
		}
		before, priced := effectivePricing(resolveModelPricingRowForTenant(tenantID, row.ModelID))
		if !priced {
			continue
		}
		after, afterPriced := effectivePricing(resolveModelPricingRow(row.ModelID, func(lookupID string) (ModelConfigRow, bool) {
			if own, ok := modelConfigStoreForTenant(tenantID).GetModelConfig(lookupID); ok {
				return own, true
			}
			if strings.ToLower(strings.TrimSpace(lookupID)) == modelKey {
				return row, true
			}
			if sys, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig(lookupID); ok {
				return sys, true
			}
			if pricing, ok := GetModelPricingForTenant(tenantID, lookupID); ok {
				return modelConfigRowFromLegacyPricing(pricing), true
			}
			return ModelConfigRow{}, false
		}))
		if !afterPriced || !samePricing(before, after) {
			return false
		}
	}
	return true
}

// catalogTenantIDs lists the tenants with catalog rows of their own: only they
// can price a model apart from the system catalog.
func catalogTenantIDs() ([]string, error) {
	db := getReadDB()
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT DISTINCT CAST(tenant_id AS TEXT) FROM model_configs
		UNION SELECT DISTINCT CAST(tenant_id AS TEXT) FROM model_pricing`)
	if err != nil {
		return nil, fmt.Errorf("usage: autofill catalog tenants: %w", err)
	}
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, normalizeTenantID(id))
	}
	return out, closeRows(rows)
}

// runModelCatalogAutofill fills what it can and queues the fill jobs.
func runModelCatalogAutofill(ctx context.Context, now time.Time) {
	targets, err := modelCatalogAutofillTargets(now)
	if err != nil {
		log.Warnf("usage: model catalog autofill: %v", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	remote, err := modelCatalogAutofillRemoteModels(ctx, now)
	if err != nil {
		log.Warnf("usage: model catalog autofill: fetch OpenRouter models: %v", err)
		return
	}
	index := autofillRemoteIndex(remote)
	tenants, err := catalogTenantIDs()
	if err != nil {
		log.Warnf("usage: model catalog autofill: %v", err)
		return
	}
	filled := 0
	for _, target := range targets {
		if ctx.Err() != nil {
			return
		}
		model, ok := autofillRemoteFor(index, target.modelID)
		if !ok {
			continue
		}
		tenantID, row, ok := autofillPlan(target, model, tenants)
		if !ok {
			continue
		}
		if err := UpsertModelConfigForTenant(tenantID, row); err != nil {
			log.Warnf("usage: model catalog autofill %s/%s: %v", tenantID, row.ModelID, err)
			continue
		}
		filled++
		log.Infof("usage: model catalog autofill priced %s for tenant %s from OpenRouter", row.ModelID, tenantID)
		if _, err := EnqueuePricingResync(tenantID, []string{row.ModelID}, PricingResyncFill, PricingResyncOriginAuto); err != nil {
			log.Warnf("usage: model catalog autofill: queue fill for %s: %v", row.ModelID, err)
		}
	}
	if filled > 0 {
		ReloadPricingCache()
	}
}

// autofillPlan decides where a target's price goes: the tenant's own row when
// it has one that no operator wrote, otherwise the system catalog, if that
// leaves every tenant's current price as it is.
func autofillPlan(target autofillTarget, model OpenRouterRemoteModel, tenants []string) (string, ModelConfigRow, bool) {
	if !isSystemTenant(target.tenantID) {
		if own, ok := modelConfigStoreForTenant(target.tenantID).GetModelConfig(target.modelID); ok {
			if strings.EqualFold(strings.TrimSpace(own.Source), "user") || !own.Enabled || hasEffectiveModelPricing(own) {
				return "", ModelConfigRow{}, false
			}
			return target.tenantID, autofillMerge(own, model), true
		}
	}
	existing, exists := modelConfigStoreForTenant(systemTenantID).GetModelConfig(target.modelID)
	if exists && (strings.EqualFold(strings.TrimSpace(existing.Source), "user") || !existing.Enabled || hasEffectiveModelPricing(existing)) {
		return "", ModelConfigRow{}, false
	}
	if !exists {
		existing = ModelConfigRow{ModelID: target.modelID, Enabled: true}
	}
	row := autofillMerge(existing, model)
	if !systemFillKeepsTenantPrices(tenants, row) {
		log.Infof("usage: model catalog autofill skipped %s: a tenant prices it through its own catalog", target.modelID)
		return "", ModelConfigRow{}, false
	}
	return systemTenantID, row, true
}

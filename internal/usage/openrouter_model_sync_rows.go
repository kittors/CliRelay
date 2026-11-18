package usage

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	sqlmodelconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/modelconfig"
)

// Writing the OpenRouter sync's rows. The sync walks some 740 listings per run.
// It used to rewrite every row it touched, whether or not anything changed (each
// write is a configsync event and a mirrored legacy price row), and to read the
// whole catalog twice per listing to find date-suffixed aliases. Once the
// database moved to another host every read became a round trip, the run no
// longer finished within its deadline, and from 11-16 every sync failed with
// "context deadline exceeded". A run now writes only rows whose values change
// and looks aliases up by prefix.

// openRouterRowForTenant reads a row the way GetModelConfigForTenant does (the
// tenant's own row, else the system catalog's) and says which it found.
func openRouterRowForTenant(tenantID, modelID string) (row ModelConfigRow, exists, inherited bool) {
	tenantID = normalizeTenantID(tenantID)
	if row, ok := modelConfigStoreForTenant(tenantID).GetModelConfig(modelID); ok {
		return row, true, false
	}
	if isSystemTenant(tenantID) {
		return ModelConfigRow{}, false, false
	}
	row, ok := modelConfigStoreForTenant(systemTenantID).GetModelConfig(modelID)
	return row, ok, ok
}

// openRouterUpsertIfChanged writes after when it differs from before. A row
// read from the system catalog for a business tenant is always written: that
// write gives the tenant its own copy, as it always has.
func openRouterUpsertIfChanged(tenantID string, before, after ModelConfigRow, inherited bool) error {
	if !inherited && modelConfigRowsEqual(before, after) {
		return nil
	}
	return UpsertModelConfigForTenant(tenantID, after)
}

// modelConfigRowsEqual compares every stored field but the write time, after
// the normalization a write applies.
func modelConfigRowsEqual(a, b ModelConfigRow) bool {
	norm := func(row ModelConfigRow) ModelConfigRow {
		row.UpdatedAt = ""
		row.ModelID = strings.TrimSpace(row.ModelID)
		row.OwnedBy = normalizeModelOwnerValue(row.OwnedBy)
		row.InputModalities = normalizeModelModalities(row.InputModalities)
		row.OutputModalities = normalizeModelModalities(row.OutputModalities)
		row.PricingMode = normalizePricingMode(row.PricingMode)
		row.DisplayName = strings.TrimSpace(row.DisplayName)
		row.KnowledgeCutoff = strings.TrimSpace(row.KnowledgeCutoff)
		row.SupportedParameters = sqlmodelconfig.NormalizeModelParameterNames(row.SupportedParameters)
		row.Reasoning = sqlmodelconfig.NormalizeModelReasoningJSON(row.Reasoning)
		if row.Source == "" {
			row.Source = "user"
		}
		return row
	}
	a, b = norm(a), norm(b)
	return a.ModelID == b.ModelID && a.OwnedBy == b.OwnedBy && a.DisplayName == b.DisplayName &&
		a.Description == b.Description && a.Enabled == b.Enabled &&
		strings.Join(a.InputModalities, "\x00") == strings.Join(b.InputModalities, "\x00") &&
		strings.Join(a.OutputModalities, "\x00") == strings.Join(b.OutputModalities, "\x00") &&
		a.ContextLength == b.ContextLength && a.MaxCompletionTokens == b.MaxCompletionTokens &&
		strings.Join(a.SupportedParameters, "\x00") == strings.Join(b.SupportedParameters, "\x00") &&
		a.Reasoning == b.Reasoning && a.KnowledgeCutoff == b.KnowledgeCutoff &&
		a.PricingMode == b.PricingMode &&
		a.InputPricePerMillion == b.InputPricePerMillion && a.OutputPricePerMillion == b.OutputPricePerMillion &&
		a.CachedPricePerMillion == b.CachedPricePerMillion && a.CacheReadPricePerMillion == b.CacheReadPricePerMillion &&
		a.CacheWritePricePerMillion == b.CacheWritePricePerMillion && a.PricePerCall == b.PricePerCall &&
		a.Source == b.Source
}

// modelConfigIDsWithPrefix lists the catalog model IDs of a tenant (its own and
// the system catalog's it inherits) that start with prefix, in the order of
// ListModelConfigsForTenant's IDs. It reads IDs only, not whole rows.
func modelConfigIDsWithPrefix(tenantID, prefix string) []string {
	db := getDB()
	if db == nil || prefix == "" {
		return nil
	}
	tenantID = normalizeTenantID(tenantID)
	tenants := []any{systemTenantID}
	if !isSystemTenant(tenantID) {
		tenants = append(tenants, tenantID)
	}
	query := `SELECT DISTINCT model_id FROM model_configs
		WHERE tenant_id IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(tenants)), ", ") + `)
		  AND substr(model_id, 1, ?) = ?`
	args := append(tenants, utf8.RuneCountInString(prefix), prefix)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return strings.ToLower(ids[i]) < strings.ToLower(ids[j]) })
	return ids
}

func openRouterSyncExistingAliasRows(tenantID, baseModelID string, model OpenRouterRemoteModel, owner string, modelIDs []string) error {
	for _, modelID := range modelIDs {
		existing, exists, inherited := openRouterRowForTenant(tenantID, modelID)
		if !exists {
			continue
		}
		if existing.Source == openRouterModelSource && !openRouterIsDateSuffixAlias(modelID, baseModelID) {
			continue
		}
		before := existing
		openRouterApplyModelSync(&existing, model, owner)
		if err := openRouterUpsertIfChanged(tenantID, before, existing, inherited); err != nil {
			return fmt.Errorf("sync openrouter model alias %s: %w", modelID, err)
		}
	}
	return nil
}

func openRouterSyncExistingWrapperRows(tenantID, baseModelID string, model OpenRouterRemoteModel) error {
	for _, wrapperID := range openRouterWrapperModelIDs(baseModelID) {
		existing, exists, inherited := openRouterRowForTenant(tenantID, wrapperID)
		if !exists {
			continue
		}
		before := existing
		switch {
		case strings.HasPrefix(wrapperID, "cline-pass/"):
			existing.OwnedBy = "cline"
		case strings.HasPrefix(wrapperID, "ollama/"):
			existing.OwnedBy = "ollama"
		}
		if description := openRouterModelDescription(model); description != "" && openRouterShouldSyncDescription(existing) {
			existing.Description = description
		}
		openRouterApplyModelSyncValues(&existing, model)
		if err := openRouterUpsertIfChanged(tenantID, before, existing, inherited); err != nil {
			return fmt.Errorf("sync openrouter model wrapper %s: %w", wrapperID, err)
		}
	}
	return nil
}

func openRouterWrapperModelIDs(baseModelID string) []string {
	baseModelID = strings.TrimSpace(baseModelID)
	if baseModelID == "" || strings.Contains(baseModelID, "/") {
		return nil
	}
	return []string{
		"cline-pass/" + baseModelID,
		"ollama/" + baseModelID,
	}
}

package modelconfig

import (
	"database/sql"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Per-tenant seeding for image and video models.
//
// seedDefaultModelConfigRows only ever writes the system tenant. Every other
// tenant's library was populated indirectly: pricing import created rows with
// source 'legacy-pricing', and repairMediaGenerationModelConfigRows later restamped
// them as 'seed'. That works for a model old enough to appear in a pricing feed and
// fails completely for a newly released one — nothing creates the row, so the model
// is invisible in the tenant's library, its catalog, its plaza and its channel-group
// editor, no matter that the runtime can serve it.
//
// gpt-image-2 reached every tenant only because it had been through that pricing
// path. gpt-image-2.5-flare and -sunburst had not, and landed nowhere but the
// system tenant.
//
// Only media models are seeded this way. Chat model availability legitimately
// differs per tenant, but an image model is servable by any credential of its
// provider, so withholding it from a tenant expresses nothing.

// seedAndRepairModelConfigRows brings the model library up to date on startup.
//
// Order matters: the system tenant is seeded first, legacy pricing rows are folded
// in and restamped, and only then are media models broadcast to the remaining
// tenants — so a row a repair step would have fixed is not first inserted fresh.
func seedAndRepairModelConfigRows(db *sql.DB) {
	seedDefaultModelConfigRows(db)
	mergeLegacyPricingIntoModelConfigs(db)
	repairDefaultPerCallModelConfigRows(db)
	repairMediaGenerationModelConfigRows(db)
	seedMediaGenerationModelsForTenants(db)
}

// seedMediaGenerationModelsForTenants gives every existing tenant a row for each
// media model in the static catalog.
//
// Existing rows are never touched: an operator's pricing, owner and enabled state
// on a model they already have must survive this.
func seedMediaGenerationModelsForTenants(db *sql.DB) {
	if db == nil {
		return
	}
	tenants := modelConfigTenantIDs(db)
	if len(tenants) == 0 {
		return
	}
	now := nowRFC3339()
	for _, row := range defaultModelConfigRows() {
		if !isMediaGenerationModel(row.ModelID) {
			continue
		}
		for _, tenantID := range tenants {
			ownedBy := tenantMediaModelOwner(db, tenantID, row.ModelID, row.OwnedBy)
			_, err := db.Exec(
				`INSERT OR IGNORE INTO model_configs
				 (tenant_id, model_id, owned_by, display_name, description, enabled, input_modalities, output_modalities, pricing_mode, input_price_per_million, output_price_per_million, cached_price_per_million, price_per_call, source, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?, ?)`,
				tenantID,
				row.ModelID,
				ownedBy,
				row.DisplayName,
				row.Description,
				boolToInt(row.Enabled),
				encodeModelModalities(row.InputModalities),
				encodeModelModalities(row.OutputModalities),
				NormalizePricingMode(row.PricingMode),
				row.PricePerCall,
				row.Source,
				now,
			)
			if err != nil {
				log.Warnf("sqlite/modelconfig: seed media model %s for tenant %s: %v", row.ModelID, tenantID, err)
			}
		}
	}
}

// modelConfigTenantIDs lists the tenants that already have a model library.
func modelConfigTenantIDs(db *sql.DB) []string {
	rows, err := db.Query(`SELECT DISTINCT tenant_id FROM model_configs WHERE tenant_id != ''`)
	if err != nil {
		log.Warnf("sqlite/modelconfig: list tenants for media seed: %v", err)
		return nil
	}
	defer func() {
		_ = rows.Close()
	}()

	tenants := make([]string, 0, 8)
	for rows.Next() {
		var tenantID string
		if err := rows.Scan(&tenantID); err != nil {
			continue
		}
		if trimmed := strings.TrimSpace(tenantID); trimmed != "" {
			tenants = append(tenants, trimmed)
		}
	}
	return tenants
}

// tenantMediaModelOwner picks the owner a new media model should carry in a tenant.
//
// Owner drives the console's owner-group mapping, and operators retarget it per
// tenant: on this deployment two tenants moved gpt-image-2 from "openai" to "codex"
// so it would show under their Codex credential group. A new model in the same
// family that kept the catalog default would silently land outside that group,
// leaving the operator to rediscover and repeat the change for every release.
//
// So a sibling already in the tenant decides it, and the catalog default applies
// only when the tenant has no sibling to follow.
func tenantMediaModelOwner(db *sql.DB, tenantID, modelID, fallback string) string {
	prefix := mediaModelFamilyPrefix(modelID)
	if prefix == "" {
		return fallback
	}
	var ownedBy string
	err := db.QueryRow(
		`SELECT owned_by FROM model_configs
		 WHERE tenant_id = ? AND model_id LIKE ? AND model_id != ? AND owned_by != ''
		 ORDER BY model_id
		 LIMIT 1`,
		tenantID,
		prefix+"%",
		modelID,
	).Scan(&ownedBy)
	if err != nil || strings.TrimSpace(ownedBy) == "" {
		return fallback
	}
	return strings.TrimSpace(ownedBy)
}

// mediaModelFamilyPrefix returns the id prefix shared by a media model's family,
// used to find a sibling whose owner a new release should follow.
func mediaModelFamilyPrefix(modelID string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	for _, prefix := range []string{"gpt-image-", "grok-imagine-image", "grok-imagine-video", "sora-", "image-"} {
		if strings.HasPrefix(normalized, prefix) {
			return prefix
		}
	}
	return ""
}

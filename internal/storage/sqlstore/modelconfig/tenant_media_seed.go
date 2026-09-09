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

// systemTenantID is the catalog tenant every other tenant's library derives from.
const systemTenantID = "00000000-0000-0000-0000-000000000001"

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

// seedMediaGenerationModelsForTenants adds newly released media models to the
// tenants that already use that model family.
//
// Deliberately not "every media model into every tenant". A tenant with no xAI
// credential has no use for grok-imagine rows, and filling its library with models
// it cannot call is noise the operator then has to sift through. The narrow rule —
// only complete a family the tenant already has — covers the case this exists for
// (gpt-image-2.5 missing from tenants that have gpt-image-2) without inventing
// entries nobody asked for.
//
// Existing rows are never touched: an operator's pricing, owner and enabled state
// on a model they already have must survive this, which matters because this runs
// on every startup.
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
			if !tenantUsesMediaModelFamily(db, tenantID, row.ModelID) {
				continue
			}
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
//
// The empty check is IS NOT NULL rather than an empty-string comparison. tenant_id is a uuid column on
// PostgreSQL, where comparing it to an empty string fails outright with
//
//	ERROR: invalid input syntax for type uuid: ""
//
// and the whole seed then silently does nothing, because the error surfaces as a
// warning on a deployment that does not log to file. SQLite types loosely and
// accepts the comparison, so the unit tests passed while production was a no-op.
// Blank ids are filtered in Go below, which behaves the same on both engines.
func modelConfigTenantIDs(db *sql.DB) []string {
	rows, err := db.Query(`SELECT DISTINCT tenant_id FROM model_configs WHERE tenant_id IS NOT NULL`)
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
// Only a sibling whose owner *differs* from the catalog default is followed, since
// that difference is the operator's decision recorded in data. Following any
// sibling would pick the wrong one: this deployment's tenant holds gpt-image-1 and
// gpt-image-1-mini on the default "openai" alongside gpt-image-2 on "codex", so the
// first sibling by id reproduces the default and misses the retarget entirely.
//
// The newest such sibling wins, on the grounds that a retarget applied to a recent
// model reflects the current intent better than one left on an older entry.
func tenantMediaModelOwner(db *sql.DB, tenantID, modelID, fallback string) string {
	prefix := mediaModelFamilyPrefix(modelID)
	if prefix == "" {
		return fallback
	}
	var ownedBy string
	err := db.QueryRow(
		`SELECT owned_by FROM model_configs
		 WHERE tenant_id = ? AND model_id LIKE ? AND model_id != ?
		   AND owned_by != '' AND owned_by != ?
		 ORDER BY model_id DESC
		 LIMIT 1`,
		tenantID,
		prefix+"%",
		modelID,
		fallback,
	).Scan(&ownedBy)
	if err != nil || strings.TrimSpace(ownedBy) == "" {
		return fallback
	}
	return strings.TrimSpace(ownedBy)
}

// tenantUsesMediaModelFamily reports whether a tenant already has any model from
// the same family, which is what makes a new release in that family relevant to it.
//
// The system tenant is always in scope: it is the catalog every tenant is derived
// from, so a model missing there is missing everywhere.
func tenantUsesMediaModelFamily(db *sql.DB, tenantID, modelID string) bool {
	if tenantID == systemTenantID {
		return true
	}
	prefix := mediaModelFamilyPrefix(modelID)
	if prefix == "" {
		return false
	}
	var present int
	err := db.QueryRow(
		`SELECT 1 FROM model_configs
		 WHERE tenant_id = ? AND model_id LIKE ? AND model_id != ?
		 LIMIT 1`,
		tenantID,
		prefix+"%",
		modelID,
	).Scan(&present)
	return err == nil
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

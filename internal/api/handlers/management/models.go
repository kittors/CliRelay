package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetModels returns the list of all available models from the global registry
// along with their pricing information.
//
// Endpoint:
//
//	GET /v0/management/models
func (h *Handler) GetModels(c *gin.Context) {
	modelRegistry := registry.GetGlobalRegistry()
	allModels := modelRegistry.GetAvailableModels("openai")

	// Get all pricing data
	pricingMap := usage.GetAllModelPricing()

	filteredModels := make([]map[string]any, len(allModels))
	for i, model := range allModels {
		filteredModel := map[string]any{
			"id":     model["id"],
			"object": model["object"],
		}
		if created, exists := model["created"]; exists {
			filteredModel["created"] = created
		}
		if ownedBy, exists := model["owned_by"]; exists {
			filteredModel["owned_by"] = ownedBy
		}

		// Attach pricing if available
		if modelID, ok := model["id"].(string); ok {
			if pricing, exists := pricingMap[modelID]; exists {
				filteredModel["pricing"] = map[string]any{
					"input_price_per_million":  pricing.InputPricePerMillion,
					"output_price_per_million": pricing.OutputPricePerMillion,
					"cached_price_per_million": pricing.CachedPricePerMillion,
				}
			}
		}

		filteredModels[i] = filteredModel
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   filteredModels,
	})
}

// GetModelPricing returns all model pricing entries.
//
// Endpoint:
//
//	GET /v0/management/model-pricing
func (h *Handler) GetModelPricing(c *gin.Context) {
	pricingMap := usage.GetAllModelPricing()

	// Convert to array for easier frontend consumption
	items := make([]map[string]any, 0, len(pricingMap))
	for _, row := range pricingMap {
		items = append(items, map[string]any{
			"model_id":                 row.ModelID,
			"input_price_per_million":  row.InputPricePerMillion,
			"output_price_per_million": row.OutputPricePerMillion,
			"cached_price_per_million": row.CachedPricePerMillion,
			"updated_at":               row.UpdatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PutModelPricing updates or creates model pricing entries in bulk.
//
// Endpoint:
//
//	PUT /v0/management/model-pricing
//
// Body: { "items": [{ "model_id": "...", "input_price_per_million": 3.0, ... }] }
func (h *Handler) PutModelPricing(c *gin.Context) {
	var body struct {
		Items []struct {
			ModelID               string  `json:"model_id"`
			InputPricePerMillion  float64 `json:"input_price_per_million"`
			OutputPricePerMillion float64 `json:"output_price_per_million"`
			CachedPricePerMillion float64 `json:"cached_price_per_million"`
		} `json:"items"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	for _, item := range body.Items {
		if item.ModelID == "" {
			continue
		}
		if err := usage.UpsertModelPricing(
			item.ModelID,
			item.InputPricePerMillion,
			item.OutputPricePerMillion,
			item.CachedPricePerMillion,
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "updated": len(body.Items)})
}

package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

var (
	clineRecommendedModelsURL    = "https://api.cline.bot/api/v1/ai/cline/recommended-models"
	clineRecommendedModelsClient = &http.Client{Timeout: 5 * time.Second}
)

// GetStaticModelDefinitions returns static model metadata for a given channel.
// Channel is provided via path param (:channel) or query param (?channel=...).
func (h *Handler) GetStaticModelDefinitions(c *gin.Context) {
	channel := strings.TrimSpace(c.Param("channel"))
	if channel == "" {
		channel = strings.TrimSpace(c.Query("channel"))
	}
	if channel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel is required"})
		return
	}

	normalizedChannel := strings.ToLower(strings.TrimSpace(channel))
	models := registry.GetStaticModelDefinitionsByChannel(normalizedChannel)
	if models == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown channel", "channel": channel})
		return
	}
	if normalizedChannel == "cline" {
		if remoteModels, err := fetchClinePassModelDefinitions(c.Request.Context()); err == nil && len(remoteModels) > 0 {
			models = remoteModels
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"channel": normalizedChannel,
		"models":  models,
	})
}

func fetchClinePassModelDefinitions(ctx context.Context) ([]*registry.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clineRecommendedModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := clineRecommendedModelsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("cline recommended models status %d", resp.StatusCode)
	}

	var payload struct {
		ClinePass []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
		} `json:"clinePass"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	models := make([]*registry.ModelInfo, 0, len(payload.ClinePass))
	for _, item := range payload.ClinePass {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		displayName := strings.TrimSpace(item.Name)
		if displayName == "" {
			displayName = id
		}
		models = append(models, &registry.ModelInfo{
			ID:          id,
			Object:      "model",
			OwnedBy:     "cline",
			Type:        "cline",
			DisplayName: displayName,
			Description: strings.TrimSpace(item.Description),
		})
	}
	return models, nil
}

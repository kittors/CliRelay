package executor

import (
	"context"
	"errors"
	"github.com/MoeForge/moego/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

const (
	claudeModelsPath          = "/v1/models"
	defaultClaudeModelsBase   = "https://api.anthropic.com"
	defaultClaudeAnthropicVer = "2023-06-01"
)

var claudeModelsCache struct {
	mu     sync.RWMutex
	models []*sdkmodelcatalog.ModelInfo
}

type claudeModelsResponse struct {
	Data   []claudeModelPayload `json:"data"`
	Models []claudeModelPayload `json:"models"`
}

type claudeModelPayload struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func cloneClaudeModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		clone := *model
		if len(model.SupportedGenerationMethods) > 0 {
			clone.SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
		}
		if len(model.SupportedParameters) > 0 {
			clone.SupportedParameters = append([]string(nil), model.SupportedParameters...)
		}
		if model.Thinking != nil {
			thinkingClone := *model.Thinking
			if len(model.Thinking.Levels) > 0 {
				thinkingClone.Levels = append([]string(nil), model.Thinking.Levels...)
			}
			clone.Thinking = &thinkingClone
		}
		out = append(out, &clone)
	}
	return out
}

func storeClaudeModels(models []*sdkmodelcatalog.ModelInfo) bool {
	cloned := cloneClaudeModels(models)
	if len(cloned) == 0 {
		return false
	}
	claudeModelsCache.mu.Lock()
	claudeModelsCache.models = cloned
	claudeModelsCache.mu.Unlock()
	return true
}

func loadClaudeModels() []*sdkmodelcatalog.ModelInfo {
	claudeModelsCache.mu.RLock()
	cloned := cloneClaudeModels(claudeModelsCache.models)
	claudeModelsCache.mu.RUnlock()
	return cloned
}

func fallbackClaudeModels() []*sdkmodelcatalog.ModelInfo {
	if models := loadClaudeModels(); len(models) > 0 {
		log.Debugf("claude executor: using cached model list (%d models)", len(models))
		return models
	}
	return nil
}

// FetchClaudeModels retrieves the live model list from Anthropic-compatible /v1/models.
// Supports OAuth (Bearer access_token) and API key (x-api-key) auth, matching request
// credential resolution used by Claude message forwarding.
func FetchClaudeModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	if ctx == nil {
		ctx = context.Background()
	}
	token, baseURL := claudeCreds(auth)
	token = strings.TrimSpace(token)
	if token == "" {
		return fallbackClaudeModels()
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultClaudeModelsBase
	}
	modelsURL := buildClaudeModelsURL(baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return fallbackClaudeModels()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", defaultClaudeAnthropicVer)

	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := strings.Contains(strings.ToLower(baseURL), "api.anthropic.com")
	if isAnthropicBase && useAPIKey {
		req.Header.Set("x-api-key", token)
	} else if useAPIKey {
		// Custom Anthropic-compatible gateways usually accept x-api-key for API keys.
		req.Header.Set("x-api-key", token)
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		// OAuth / setup-token style: Bearer access_token + oauth beta, aligned with sub2api.
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}

	resp, err := newProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Debugf("claude executor: models request failed: %v", err)
		}
		return fallbackClaudeModels()
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("claude executor: close models response body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		log.Debugf("claude executor: models request failed with status %d", resp.StatusCode)
		return fallbackClaudeModels()
	}

	body, err := readUpstreamResponseBody("claude", resp.Body)
	if err != nil {
		log.Debugf("claude executor: models response read failed: %v", err)
		return fallbackClaudeModels()
	}

	models, ok := parseClaudeModels(body, time.Now().Unix())
	if !ok {
		log.Debug("claude executor: fetched empty or invalid model list; retaining cached model list")
		return fallbackClaudeModels()
	}
	storeClaudeModels(models)
	return models
}

func buildClaudeModelsURL(base string) string {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(strings.ToLower(normalized), "/v1/models") {
		return normalized
	}
	if strings.HasSuffix(strings.ToLower(normalized), "/v1") {
		return normalized + "/models"
	}
	return normalized + claudeModelsPath
}

func parseClaudeModels(body []byte, now int64) ([]*sdkmodelcatalog.ModelInfo, bool) {
	var decoded claudeModelsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		var arrayResponse []claudeModelPayload
		if arrayErr := json.Unmarshal(body, &arrayResponse); arrayErr != nil {
			return nil, false
		}
		decoded.Data = arrayResponse
	}

	entries := decoded.Data
	if len(entries) == 0 {
		entries = decoded.Models
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, item := range entries {
		modelID := strings.TrimSpace(item.ID)
		if modelID == "" {
			continue
		}
		key := strings.ToLower(modelID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		displayName := strings.TrimSpace(item.DisplayName)
		if displayName == "" {
			displayName = modelID
		}
		object := strings.TrimSpace(item.Type)
		if object == "" {
			object = "model"
		}
		model := &sdkmodelcatalog.ModelInfo{
			ID:          modelID,
			Object:      object,
			Created:     now,
			OwnedBy:     "claude",
			Type:        "claude",
			DisplayName: displayName,
			Name:        modelID,
			Version:     modelID,
		}
		if static := sdkmodelcatalog.LookupStaticModelInfo(modelID); static != nil {
			if strings.TrimSpace(static.Description) != "" {
				model.Description = static.Description
			}
			if strings.TrimSpace(static.DisplayName) != "" {
				model.DisplayName = static.DisplayName
			}
			if static.Thinking != nil {
				thinkingClone := *static.Thinking
				if len(static.Thinking.Levels) > 0 {
					thinkingClone.Levels = append([]string(nil), static.Thinking.Levels...)
				}
				model.Thinking = &thinkingClone
			}
			if static.ContextLength > 0 {
				model.ContextLength = static.ContextLength
				model.InputTokenLimit = static.ContextLength
			}
			if static.MaxCompletionTokens > 0 {
				model.MaxCompletionTokens = static.MaxCompletionTokens
				model.OutputTokenLimit = static.MaxCompletionTokens
			}
		}
		out = append(out, model)
	}
	return out, len(out) > 0
}

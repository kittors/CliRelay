package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

var antigravityPrimaryModelsCache struct {
	mu     sync.RWMutex
	models []*sdkmodelcatalog.ModelInfo
}

func cloneAntigravityModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		out = append(out, cloneAntigravityModelInfo(model))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneAntigravityModelInfo(model *sdkmodelcatalog.ModelInfo) *sdkmodelcatalog.ModelInfo {
	if model == nil {
		return nil
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
	return &clone
}

func storeAntigravityPrimaryModels(models []*sdkmodelcatalog.ModelInfo) bool {
	cloned := cloneAntigravityModels(models)
	if len(cloned) == 0 {
		return false
	}
	antigravityPrimaryModelsCache.mu.Lock()
	antigravityPrimaryModelsCache.models = cloned
	antigravityPrimaryModelsCache.mu.Unlock()
	return true
}

func loadAntigravityPrimaryModels() []*sdkmodelcatalog.ModelInfo {
	antigravityPrimaryModelsCache.mu.RLock()
	cloned := cloneAntigravityModels(antigravityPrimaryModelsCache.models)
	antigravityPrimaryModelsCache.mu.RUnlock()
	return cloned
}

func fallbackAntigravityPrimaryModels() []*sdkmodelcatalog.ModelInfo {
	models := loadAntigravityPrimaryModels()
	if len(models) > 0 {
		log.Debugf("antigravity executor: using cached primary model list (%d models)", len(models))
	}
	return models
}

// FetchAntigravityModels retrieves available models using the supplied auth.
func FetchAntigravityModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	exec := &AntigravityExecutor{cfg: cfg}
	token, updatedAuth, errToken := exec.ensureAccessToken(ctx, auth)
	if errToken != nil || token == "" {
		return fallbackAntigravityPrimaryModels()
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, cfg, auth, 0)

	for idx, baseURL := range baseURLs {
		modelsURL := baseURL + antigravityModelsPath
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, modelsURL, bytes.NewReader(antigravityModelsRequestPayload(auth)))
		if errReq != nil {
			return fallbackAntigravityPrimaryModels()
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return fallbackAntigravityPrimaryModels()
			}
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return fallbackAntigravityPrimaryModels()
		}

		readBody := readUpstreamResponseBody
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			readBody = func(provider string, r io.Reader) ([]byte, error) {
				return readUpstreamErrorBody(provider, r), nil
			}
		}
		bodyBytes, errRead := readBody("antigravity", httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models read error on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return fallbackAntigravityPrimaryModels()
		}
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request rate limited on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models request failed with status %d on base url %s, retrying with fallback base url: %s", httpResp.StatusCode, baseURL, baseURLs[idx+1])
				continue
			}
			return fallbackAntigravityPrimaryModels()
		}

		result := gjson.GetBytes(bodyBytes, "models")
		if !result.Exists() {
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: models field missing on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return fallbackAntigravityPrimaryModels()
		}

		now := time.Now().Unix()
		modelConfig := registry.GetAntigravityModelConfig()
		models := make([]*sdkmodelcatalog.ModelInfo, 0, len(result.Map()))
		for originalName, modelData := range result.Map() {
			modelID := strings.TrimSpace(originalName)
			if modelID == "" {
				continue
			}
			if isInternalAntigravityModel(modelID, modelData) {
				continue
			}
			modelCfg := modelConfig[modelID]

			displayName := modelData.Get("displayName").String()
			if displayName == "" {
				displayName = modelID
			}

			modelInfo := &sdkmodelcatalog.ModelInfo{
				ID:          modelID,
				Name:        modelID,
				Description: displayName,
				DisplayName: displayName,
				Version:     modelID,
				Object:      "model",
				Created:     now,
				OwnedBy:     antigravityAuthType,
				Type:        antigravityAuthType,
			}
			if maxTokens := modelData.Get("maxTokens").Int(); maxTokens > 0 {
				modelInfo.ContextLength = int(maxTokens)
				modelInfo.InputTokenLimit = int(maxTokens)
			}
			if maxOutputTokens := modelData.Get("maxOutputTokens").Int(); maxOutputTokens > 0 {
				modelInfo.MaxCompletionTokens = int(maxOutputTokens)
				modelInfo.OutputTokenLimit = int(maxOutputTokens)
			}
			if modelCfg != nil {
				if modelCfg.Thinking != nil {
					modelInfo.Thinking = &sdkmodelcatalog.ThinkingSupport{
						Min:            modelCfg.Thinking.Min,
						Max:            modelCfg.Thinking.Max,
						ZeroAllowed:    modelCfg.Thinking.ZeroAllowed,
						DynamicAllowed: modelCfg.Thinking.DynamicAllowed,
						Levels:         append([]string(nil), modelCfg.Thinking.Levels...),
					}
				}
				if modelCfg.MaxCompletionTokens > 0 {
					modelInfo.MaxCompletionTokens = modelCfg.MaxCompletionTokens
				}
			} else if strings.Contains(strings.ToLower(modelID), "claude") || strings.Contains(strings.ToLower(modelID), "gemini-3") {
				// Dynamic thinking defaults for newer discovered models
				modelInfo.Thinking = &sdkmodelcatalog.ThinkingSupport{
					DynamicAllowed: true,
					Levels:         []string{"low", "medium", "high"},
				}
			}
			models = append(models, modelInfo)
		}
		if len(models) == 0 {
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity executor: empty models list on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
				continue
			}
			log.Debug("antigravity executor: fetched empty model list; retaining cached primary model list")
			return fallbackAntigravityPrimaryModels()
		}
		storeAntigravityPrimaryModels(models)
		return models
	}
	return fallbackAntigravityPrimaryModels()
}

func antigravityModelsRequestPayload(auth *cliproxyauth.Auth) []byte {
	projectID := ""
	if auth != nil {
		if auth.Metadata != nil {
			projectID = metaStringValue(auth.Metadata, "project_id")
			if projectID == "" {
				projectID = metaStringValue(auth.Metadata, "project")
			}
		}
		if projectID == "" && auth.Attributes != nil {
			projectID = strings.TrimSpace(auth.Attributes["project_id"])
			if projectID == "" {
				projectID = strings.TrimSpace(auth.Attributes["project"])
			}
		}
	}
	if projectID == "" {
		return []byte(`{}`)
	}
	payload, err := json.Marshal(map[string]string{"project": projectID})
	if err != nil {
		return []byte(`{}`)
	}
	return payload
}

func isInternalAntigravityModel(modelID string, modelData gjson.Result) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return true
	}
	if modelData.Get("isInternal").Bool() {
		return true
	}
	return strings.HasPrefix(id, "chat_") ||
		strings.HasPrefix(id, "tab_") ||
		strings.HasPrefix(id, "tab-jump") ||
		strings.HasPrefix(id, "tab_jump")
}

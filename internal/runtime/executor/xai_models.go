package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

const (
	xaiModelsPath                  = "/models"
	xaiGrokBuildClientModelAlias   = "grok-build"
	xaiGrokBuildUpstreamModelAlias = "grok-build-0.1"
)

var xaiModelsCache struct {
	mu     sync.RWMutex
	models []*sdkmodelcatalog.ModelInfo
}

type xaiModelsResponse struct {
	Data []xaiModelPayload `json:"data"`
}

type xaiModelPayload struct {
	ID                  string   `json:"id"`
	Aliases             []string `json:"aliases"`
	Object              string   `json:"object"`
	Created             int64    `json:"created"`
	OwnedBy             string   `json:"owned_by"`
	ContextLength       int      `json:"context_length"`
	MaxCompletionTokens int      `json:"max_completion_tokens"`
}

func cloneXAIModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
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

func storeXAIModels(models []*sdkmodelcatalog.ModelInfo) bool {
	cloned := cloneXAIModels(models)
	if len(cloned) == 0 {
		return false
	}
	xaiModelsCache.mu.Lock()
	xaiModelsCache.models = cloned
	xaiModelsCache.mu.Unlock()
	return true
}

func loadXAIModels() []*sdkmodelcatalog.ModelInfo {
	xaiModelsCache.mu.RLock()
	cloned := cloneXAIModels(xaiModelsCache.models)
	xaiModelsCache.mu.RUnlock()
	return cloned
}

func fallbackXAIModels() []*sdkmodelcatalog.ModelInfo {
	if models := loadXAIModels(); len(models) > 0 {
		log.Debugf("xai executor: using cached model list (%d models)", len(models))
		return models
	}
	return nil
}

// FetchXAIModels retrieves the OAuth account's live model list from xAI.
func FetchXAIModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	if ctx == nil {
		ctx = context.Background()
	}
	token, baseURL := xaiCreds(auth)
	if strings.TrimSpace(token) == "" {
		return fallbackXAIModels()
	}
	if baseURL == "" {
		baseURL = xaiauth.DefaultAPIBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+xaiModelsPath, nil)
	if err != nil {
		return fallbackXAIModels()
	}
	applyXAIHeaders(req, cfg, auth, token, false)

	resp, err := newProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Debugf("xai executor: models request failed: %v", err)
		}
		return fallbackXAIModels()
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close models response body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		log.Debugf("xai executor: models request failed with status %d", resp.StatusCode)
		return fallbackXAIModels()
	}

	body, err := readUpstreamResponseBody("xai", resp.Body)
	if err != nil {
		log.Debugf("xai executor: models response read failed: %v", err)
		return fallbackXAIModels()
	}

	models, ok := parseXAIModels(body, time.Now().Unix())
	if !ok {
		log.Debug("xai executor: fetched empty or invalid model list; retaining cached model list")
		return fallbackXAIModels()
	}
	storeXAIModels(models)
	return models
}

func parseXAIModels(body []byte, now int64) ([]*sdkmodelcatalog.ModelInfo, bool) {
	var decoded xaiModelsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(decoded.Data))
	seen := make(map[string]struct{}, len(decoded.Data))
	for _, item := range decoded.Data {
		modelID := strings.TrimSpace(item.ID)
		if modelID == "" {
			continue
		}
		model := xaiModelInfoFromPayload(item, modelID, now)
		addXAIModel(&out, seen, model)
		for _, alias := range item.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" || strings.EqualFold(alias, modelID) {
				continue
			}
			clone := *model
			clone.ID = alias
			clone.Name = alias
			clone.UpstreamModelID = modelID
			addXAIModel(&out, seen, &clone)
		}
	}
	out = withXAIGrokBuildCompatibilityAlias(out)
	return out, len(out) > 0
}

func xaiModelInfoFromPayload(item xaiModelPayload, modelID string, now int64) *sdkmodelcatalog.ModelInfo {
	object := strings.TrimSpace(item.Object)
	if object == "" {
		object = "model"
	}
	ownedBy := strings.TrimSpace(item.OwnedBy)
	if ownedBy == "" {
		ownedBy = "xai"
	}
	created := item.Created
	if created == 0 {
		created = now
	}
	model := &sdkmodelcatalog.ModelInfo{
		ID:                  modelID,
		Object:              object,
		Created:             created,
		OwnedBy:             ownedBy,
		Type:                "xai",
		DisplayName:         modelID,
		Name:                modelID,
		Version:             modelID,
		ContextLength:       item.ContextLength,
		InputTokenLimit:     item.ContextLength,
		MaxCompletionTokens: item.MaxCompletionTokens,
		OutputTokenLimit:    item.MaxCompletionTokens,
	}
	if static := sdkmodelcatalog.LookupStaticModelInfo(modelID); static != nil {
		model.Description = static.Description
		model.DisplayName = firstNonEmptyString(static.DisplayName, model.DisplayName)
		model.Thinking = cloneXAIThinking(static.Thinking)
	}
	return model
}

func addXAIModel(models *[]*sdkmodelcatalog.ModelInfo, seen map[string]struct{}, model *sdkmodelcatalog.ModelInfo) {
	if model == nil {
		return
	}
	id := strings.TrimSpace(model.ID)
	if id == "" {
		return
	}
	key := strings.ToLower(id)
	if _, exists := seen[key]; exists {
		return
	}
	seen[key] = struct{}{}
	*models = append(*models, model)
}

func withXAIGrokBuildCompatibilityAlias(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
	if len(models) == 0 {
		return models
	}
	var upstream *sdkmodelcatalog.ModelInfo
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if strings.EqualFold(id, xaiGrokBuildClientModelAlias) {
			return models
		}
		if strings.EqualFold(id, xaiGrokBuildUpstreamModelAlias) {
			upstream = model
		}
	}
	if upstream == nil {
		return models
	}
	clone := *upstream
	clone.ID = xaiGrokBuildClientModelAlias
	clone.Name = xaiGrokBuildClientModelAlias
	clone.UpstreamModelID = strings.TrimSpace(upstream.ID)
	clone.DisplayName = "Grok Build"
	return append(models, &clone)
}

func cloneXAIThinking(thinking *sdkmodelcatalog.ThinkingSupport) *sdkmodelcatalog.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	clone := *thinking
	if len(thinking.Levels) > 0 {
		clone.Levels = append([]string(nil), thinking.Levels...)
	}
	return &clone
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

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

	kimiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

const (
	kimiModelsPath = "/v1/models"
	// Upstream advertises bare ids (k2.5); routing uses the kimi- namespace and
	// stripKimiPrefix removes it again before the chat request goes out. Keeping
	// discovery in the prefixed namespace is what makes a newly released id
	// routable without a catalog change.
	kimiModelIDPrefix = "kimi-"
)

var kimiModelsCache struct {
	mu     sync.RWMutex
	models []*sdkmodelcatalog.ModelInfo
}

type kimiModelsResponse struct {
	Data []kimiModelPayload `json:"data"`
}

type kimiModelPayload struct {
	ID                  string `json:"id"`
	Object              string `json:"object"`
	Created             int64  `json:"created"`
	OwnedBy             string `json:"owned_by"`
	DisplayName         string `json:"display_name"`
	Description         string `json:"description"`
	ContextLength       int    `json:"context_length"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
}

func cloneKimiModels(models []*sdkmodelcatalog.ModelInfo) []*sdkmodelcatalog.ModelInfo {
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
		clone.Thinking = cloneKimiThinking(model.Thinking)
		out = append(out, &clone)
	}
	return out
}

func storeKimiModels(models []*sdkmodelcatalog.ModelInfo) bool {
	cloned := cloneKimiModels(models)
	if len(cloned) == 0 {
		return false
	}
	kimiModelsCache.mu.Lock()
	kimiModelsCache.models = cloned
	kimiModelsCache.mu.Unlock()
	return true
}

func loadKimiModels() []*sdkmodelcatalog.ModelInfo {
	kimiModelsCache.mu.RLock()
	cloned := cloneKimiModels(kimiModelsCache.models)
	kimiModelsCache.mu.RUnlock()
	return cloned
}

// fallbackKimiModels keeps the last successful discovery result. It deliberately
// does not fall back to the compiled-in catalog: callers that need a guaranteed
// list (startup registration) apply that fallback themselves, so an empty return
// here still means "upstream told us nothing".
func fallbackKimiModels() []*sdkmodelcatalog.ModelInfo {
	if models := loadKimiModels(); len(models) > 0 {
		log.Debugf("kimi executor: using cached model list (%d models)", len(models))
		return models
	}
	return nil
}

// FetchKimiModels retrieves the account's live model list from the Kimi coding
// gateway, which exposes the OpenAI-compatible /v1/models listing on the same
// base URL and credentials as chat completions.
func FetchKimiModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*sdkmodelcatalog.ModelInfo {
	if ctx == nil {
		ctx = context.Background()
	}
	token := strings.TrimSpace(kimiCreds(auth))
	if token == "" {
		return fallbackKimiModels()
	}

	modelsURL := strings.TrimRight(kimiauth.KimiAPIBaseURL, "/") + kimiModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return fallbackKimiModels()
	}
	// Same identity headers as chat traffic: the gateway rejects requests that do
	// not look like kimi-cli, and the device id must match the account's.
	applyKimiHeadersWithAuth(req, token, false, auth, cfg)

	resp, err := newProxyAwareHTTPClient(ctx, cfg, auth, 0).Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Debugf("kimi executor: models request failed: %v", err)
		}
		return fallbackKimiModels()
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close models response body error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		log.Debugf("kimi executor: models request failed with status %d", resp.StatusCode)
		return fallbackKimiModels()
	}

	body, err := readUpstreamResponseBody("kimi", resp.Body)
	if err != nil {
		log.Debugf("kimi executor: models response read failed: %v", err)
		return fallbackKimiModels()
	}

	models, ok := parseKimiModels(body, time.Now().Unix())
	if !ok {
		log.Debug("kimi executor: fetched empty or invalid model list; retaining cached model list")
		return fallbackKimiModels()
	}
	storeKimiModels(models)
	return models
}

func parseKimiModels(body []byte, now int64) ([]*sdkmodelcatalog.ModelInfo, bool) {
	var decoded kimiModelsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(decoded.Data))
	seen := make(map[string]struct{}, len(decoded.Data))
	for _, item := range decoded.Data {
		upstreamID := strings.TrimSpace(item.ID)
		if upstreamID == "" {
			continue
		}
		model := kimiModelInfoFromPayload(item, upstreamID, now)
		key := strings.ToLower(model.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out, len(out) > 0
}

func kimiModelInfoFromPayload(item kimiModelPayload, upstreamID string, now int64) *sdkmodelcatalog.ModelInfo {
	modelID := namespaceKimiModelID(upstreamID)
	object := strings.TrimSpace(item.Object)
	if object == "" {
		object = "model"
	}
	ownedBy := strings.TrimSpace(item.OwnedBy)
	if ownedBy == "" {
		ownedBy = "moonshot"
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
		Type:                "kimi",
		DisplayName:         firstNonEmptyString(item.DisplayName, kimiDisplayNameFromID(modelID)),
		Name:                modelID,
		Version:             modelID,
		Description:         strings.TrimSpace(item.Description),
		ContextLength:       item.ContextLength,
		InputTokenLimit:     item.ContextLength,
		MaxCompletionTokens: item.MaxCompletionTokens,
		OutputTokenLimit:    item.MaxCompletionTokens,
	}
	if !strings.EqualFold(modelID, upstreamID) {
		model.UpstreamModelID = upstreamID
	}
	// The listing carries ids, not capabilities. Reasoning support and token
	// limits come from the compiled-in catalog when it knows the model; a model
	// the catalog has never seen keeps Thinking nil rather than inheriting a
	// guessed budget, because an unsupported reasoning_effort is a 400 upstream.
	if static := sdkmodelcatalog.LookupStaticModelInfo(modelID); static != nil {
		model.Thinking = cloneKimiThinking(static.Thinking)
		if model.Description == "" {
			model.Description = static.Description
		}
		model.DisplayName = firstNonEmptyString(item.DisplayName, static.DisplayName, model.DisplayName)
		if model.ContextLength == 0 {
			model.ContextLength = static.ContextLength
			model.InputTokenLimit = static.ContextLength
		}
		if model.MaxCompletionTokens == 0 {
			model.MaxCompletionTokens = static.MaxCompletionTokens
			model.OutputTokenLimit = static.MaxCompletionTokens
		}
	}
	return model
}

// namespaceKimiModelID puts an upstream id into the kimi- routing namespace.
// Ids that already carry the prefix are left alone so the gateway switching to
// prefixed ids would not produce kimi-kimi-k2.
func namespaceKimiModelID(upstreamID string) string {
	trimmed := strings.TrimSpace(upstreamID)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), kimiModelIDPrefix) {
		return trimmed
	}
	return kimiModelIDPrefix + trimmed
}

// kimiDisplayNameFromID renders kimi-k2.5 as "Kimi K2.5" so a model the catalog
// does not know still reads like the curated entries in the panel.
func kimiDisplayNameFromID(modelID string) string {
	trimmed := strings.TrimSpace(modelID)
	if trimmed == "" {
		return ""
	}
	parts := strings.Split(trimmed, "-")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

func cloneKimiThinking(thinking *sdkmodelcatalog.ThinkingSupport) *sdkmodelcatalog.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	clone := *thinking
	if len(thinking.Levels) > 0 {
		clone.Levels = append([]string(nil), thinking.Levels...)
	}
	return &clone
}

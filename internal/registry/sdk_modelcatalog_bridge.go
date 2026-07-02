package registry

import (
	"context"

	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

type sdkModelCatalogRegistry struct {
	inner *ModelRegistry
}

func (r sdkModelCatalogRegistry) RegisterClient(clientID, clientProvider string, models []*sdkmodelcatalog.ModelInfo) {
	if r.inner == nil {
		return
	}
	r.inner.RegisterClient(clientID, clientProvider, cloneSDKModelInfosToInternal(models))
}

func (r sdkModelCatalogRegistry) UnregisterClient(clientID string) {
	if r.inner == nil {
		return
	}
	r.inner.UnregisterClient(clientID)
}

func (r sdkModelCatalogRegistry) SetModelQuotaExceeded(clientID, modelID string) {
	if r.inner == nil {
		return
	}
	r.inner.SetModelQuotaExceeded(clientID, modelID)
}

func (r sdkModelCatalogRegistry) SuspendClientModel(clientID, modelID string, reason string) {
	if r.inner == nil {
		return
	}
	r.inner.SuspendClientModel(clientID, modelID, reason)
}

func (r sdkModelCatalogRegistry) ResumeClientModel(clientID, modelID string) {
	if r.inner == nil {
		return
	}
	r.inner.ResumeClientModel(clientID, modelID)
}

func (r sdkModelCatalogRegistry) ClearModelQuotaExceeded(clientID, modelID string) {
	if r.inner == nil {
		return
	}
	r.inner.ClearModelQuotaExceeded(clientID, modelID)
}

func (r sdkModelCatalogRegistry) ClientSupportsModel(clientID, modelID string) bool {
	if r.inner == nil {
		return false
	}
	return r.inner.ClientSupportsModel(clientID, modelID)
}

func (r sdkModelCatalogRegistry) GetAvailableModels(handlerType string) []map[string]any {
	if r.inner == nil {
		return nil
	}
	return r.inner.GetAvailableModels(handlerType)
}

func (r sdkModelCatalogRegistry) GetAvailableModelsByProvider(provider string) []*sdkmodelcatalog.ModelInfo {
	if r.inner == nil {
		return nil
	}
	return cloneInternalModelInfosToSDK(r.inner.GetAvailableModelsByProvider(provider))
}

func (r sdkModelCatalogRegistry) GetModelProviders(modelID string) []string {
	if r.inner == nil {
		return nil
	}
	return r.inner.GetModelProviders(modelID)
}

func (r sdkModelCatalogRegistry) GetFirstAvailableModel(handlerType string) (string, error) {
	if r.inner == nil {
		return "", nil
	}
	return r.inner.GetFirstAvailableModel(handlerType)
}

func (r sdkModelCatalogRegistry) GetModelsForClient(clientID string) []*sdkmodelcatalog.ModelInfo {
	if r.inner == nil {
		return nil
	}
	return cloneInternalModelInfosToSDK(r.inner.GetModelsForClient(clientID))
}

func (r sdkModelCatalogRegistry) SetHook(hook sdkmodelcatalog.RegistryHook) {
	if r.inner == nil {
		return
	}
	if hook == nil {
		r.inner.SetHook(nil)
		return
	}
	r.inner.SetHook(sdkModelCatalogHookAdapter{inner: hook})
}

type sdkModelCatalogHookAdapter struct {
	inner sdkmodelcatalog.RegistryHook
}

func (a sdkModelCatalogHookAdapter) OnModelsRegistered(ctx context.Context, provider, clientID string, models []*ModelInfo) {
	if a.inner == nil {
		return
	}
	a.inner.OnModelsRegistered(ctx, provider, clientID, cloneInternalModelInfosToSDK(models))
}

func (a sdkModelCatalogHookAdapter) OnModelsUnregistered(ctx context.Context, provider, clientID string) {
	if a.inner == nil {
		return
	}
	a.inner.OnModelsUnregistered(ctx, provider, clientID)
}

type sdkStaticCatalogBridge struct{}

func (sdkStaticCatalogBridge) StaticModelDefinitionsByChannel(channel string) []*sdkmodelcatalog.ModelInfo {
	return cloneInternalModelInfosToSDK(GetStaticModelDefinitionsByChannel(channel))
}

func (sdkStaticCatalogBridge) LookupStaticModelInfo(modelID string) *sdkmodelcatalog.ModelInfo {
	return cloneInternalModelInfoToSDK(LookupStaticModelInfo(modelID))
}

func cloneInternalModelInfosToSDK(models []*ModelInfo) []*sdkmodelcatalog.ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, cloneInternalModelInfoToSDK(model))
	}
	return out
}

func cloneInternalModelInfoToSDK(model *ModelInfo) *sdkmodelcatalog.ModelInfo {
	if model == nil {
		return nil
	}
	return &sdkmodelcatalog.ModelInfo{
		ID:                         model.ID,
		Object:                     model.Object,
		Created:                    model.Created,
		OwnedBy:                    model.OwnedBy,
		Type:                       model.Type,
		DisplayName:                model.DisplayName,
		UpstreamModelID:            model.UpstreamModelID,
		Name:                       model.Name,
		Version:                    model.Version,
		Description:                model.Description,
		InputTokenLimit:            model.InputTokenLimit,
		OutputTokenLimit:           model.OutputTokenLimit,
		SupportedGenerationMethods: append([]string(nil), model.SupportedGenerationMethods...),
		ContextLength:              model.ContextLength,
		MaxCompletionTokens:        model.MaxCompletionTokens,
		SupportedParameters:        append([]string(nil), model.SupportedParameters...),
		Thinking:                   cloneInternalThinkingSupportToSDK(model.Thinking),
		UserDefined:                model.UserDefined,
	}
}

func cloneInternalThinkingSupportToSDK(thinking *ThinkingSupport) *sdkmodelcatalog.ThinkingSupport {
	if thinking == nil {
		return nil
	}
	return &sdkmodelcatalog.ThinkingSupport{
		Min:            thinking.Min,
		Max:            thinking.Max,
		ZeroAllowed:    thinking.ZeroAllowed,
		DynamicAllowed: thinking.DynamicAllowed,
		Levels:         append([]string(nil), thinking.Levels...),
	}
}

func cloneSDKModelInfosToInternal(models []*sdkmodelcatalog.ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		out = append(out, cloneSDKModelInfoToInternal(model))
	}
	return out
}

func cloneSDKModelInfoToInternal(model *sdkmodelcatalog.ModelInfo) *ModelInfo {
	if model == nil {
		return nil
	}
	return &ModelInfo{
		ID:                         model.ID,
		Object:                     model.Object,
		Created:                    model.Created,
		OwnedBy:                    model.OwnedBy,
		Type:                       model.Type,
		DisplayName:                model.DisplayName,
		UpstreamModelID:            model.UpstreamModelID,
		Name:                       model.Name,
		Version:                    model.Version,
		Description:                model.Description,
		InputTokenLimit:            model.InputTokenLimit,
		OutputTokenLimit:           model.OutputTokenLimit,
		SupportedGenerationMethods: append([]string(nil), model.SupportedGenerationMethods...),
		ContextLength:              model.ContextLength,
		MaxCompletionTokens:        model.MaxCompletionTokens,
		SupportedParameters:        append([]string(nil), model.SupportedParameters...),
		Thinking:                   cloneSDKThinkingSupportToInternal(model.Thinking),
		UserDefined:                model.UserDefined,
	}
}

func cloneSDKThinkingSupportToInternal(thinking *sdkmodelcatalog.ThinkingSupport) *ThinkingSupport {
	if thinking == nil {
		return nil
	}
	return &ThinkingSupport{
		Min:            thinking.Min,
		Max:            thinking.Max,
		ZeroAllowed:    thinking.ZeroAllowed,
		DynamicAllowed: thinking.DynamicAllowed,
		Levels:         append([]string(nil), thinking.Levels...),
	}
}

func init() {
	sdkmodelcatalog.SetGlobalRegistryProvider(func() sdkmodelcatalog.Registry {
		return sdkModelCatalogRegistry{inner: GetGlobalRegistry()}
	})
	sdkmodelcatalog.SetStaticCatalogProvider(func() sdkmodelcatalog.StaticCatalog {
		return sdkStaticCatalogBridge{}
	})
}

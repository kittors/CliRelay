package cliproxy

import (
	"context"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v8/sdkbridge/service"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v8/sdk/modelcatalog"
)

func init() {
	coreauth.SetDefaultModelRegistryProvider(func() coreauth.ModelRegistry {
		return sdkmodelcatalog.GlobalRegistry()
	})
}

func lookupStaticModelThinking(name string) *ModelThinkingSupport {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	upstream := internalregistry.LookupStaticModelInfo(name)
	if upstream == nil {
		return nil
	}
	return upstream.Thinking
}

// lookupBedrockStaticModelThinking is lookupStaticModelThinking for the names
// on a Bedrock key, which may be Bedrock ids ("us.anthropic.claude-opus-4-7-v1")
// that the catalog lists under their Anthropic id. Without it such a model has
// no thinking capabilities, its requests skip the adaptive-only conversion and
// a budget reaches a model that rejects it.
func lookupBedrockStaticModelThinking(name string) *ModelThinkingSupport {
	if thinking := lookupStaticModelThinking(name); thinking != nil {
		return thinking
	}
	for _, candidate := range sdkmodelcatalog.ClaudeModelCandidatesForBedrockID(name) {
		if thinking := lookupStaticModelThinking(candidate); thinking != nil {
			return thinking
		}
	}
	return nil
}

// oauthCatalogScope returns the catalog rows and mapped model owners that make a
// tenant's model-library entries routable for this credential. Both are scoped to
// the credential's own tenant: registration must not leak (or depend on) another
// tenant's library.
func oauthCatalogScope(a *coreauth.Auth) ([]oauthProviderModelConfigRow, []string) {
	if a == nil {
		return nil, nil
	}
	rows := listOAuthProviderModelConfigRowsForTenant(a.TenantID)
	if len(rows) == 0 {
		return nil, nil
	}
	groups := append([]string{a.Provider, a.ChannelName()}, a.ChannelIdentifiers()...)
	return rows, serviceapp.ListModelOwnersForAuthGroupsForTenant(a.TenantID, groups)
}

func (s *Service) refreshRegisteredModels(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, auth := range s.coreManager.List() {
		s.registerModelsForAuth(ctx, auth)
	}
}

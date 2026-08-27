package cliproxy

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
)

// fetchKimiRegistryModels resolves the models a Kimi account can route.
//
// Discovery comes first so a newly released id is routable the moment the coding
// gateway advertises it, instead of waiting for a catalog release. The compiled-in
// catalog stays as the floor: a gateway outage or a credential that cannot list
// models must not deregister every model on an otherwise working account.
func (s *Service) fetchKimiRegistryModels(ctx context.Context, auth *coreauth.Auth, excluded []string) []*ModelInfo {
	fetchCtx := ctx
	if fetchCtx == nil {
		fetchCtx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(fetchCtx), 15*time.Second)
	defer cancel()
	models := serviceapp.FetchKimiModels(fetchCtx, auth, s.cfg)
	if len(models) == 0 {
		models = sdkmodelcatalog.StaticModelDefinitionsByChannel("kimi")
	}
	return applyExcludedModels(models, excluded)
}

package cliproxy

import (
	"context"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
)

func (s *Service) fetchXAIRegistryModels(ctx context.Context, auth *coreauth.Auth, excluded []string) []*ModelInfo {
	fetchCtx := ctx
	if fetchCtx == nil {
		fetchCtx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(fetchCtx), 15*time.Second)
	defer cancel()
	models := serviceapp.FetchXAIModels(fetchCtx, auth, s.cfg)
	return applyExcludedModels(models, excluded)
}

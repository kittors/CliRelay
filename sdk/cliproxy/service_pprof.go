package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type pprofRuntime interface {
	Apply(cfg *config.Config)
	Shutdown(ctx context.Context) error
}

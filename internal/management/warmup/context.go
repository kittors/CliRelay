package warmup

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// warmupRequestLabel stands in for an API key in the request log: a warmup is
// sent by the relay itself, not by a client.
const warmupRequestLabel = "POST /auth-files/warmup"

// warmupContext is the context a warmup request runs under. It names the
// account's own tenant as the request's tenant, so the request log row, its cost
// and the dashboards land with the tenant whose account was warmed; without it
// the row has no key to resolve a tenant from and is filed under the system
// tenant.
func warmupContext(ctx context.Context, auth *coreauth.Auth) context.Context {
	ctx = context.WithValue(ctx, util.ContextKeyAPIKey, warmupRequestLabel)
	if auth != nil {
		ctx = context.WithValue(ctx, util.ContextKeyTrustedTenantID, coreauth.NormalizedTenantID(auth.TenantID))
	}
	return ctx
}

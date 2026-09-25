package identity

import (
	"context"
	"database/sql"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// commitTenantChange commits a transaction that created, changed or disabled
// a tenant and announces it, so every node rebuilds its per-tenant runtime
// configuration (a tenant created on one node must be routable on all).
func commitTenantChange(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return configsync.CommitTx(ctx, tx, configsync.Event(configsync.DomainTenants, tenantID))
}

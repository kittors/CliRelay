package enduser

import (
	"context"
	"database/sql"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
)

// commitKeyChange commits a transaction that changed API keys. It bumps the
// api_keys collection version, so a full replacement the panel computed from
// an older list is rejected, and announces the change so that every node
// rebuilds the key map it authenticates requests against: a key rotated or
// deleted here must stop working on the other nodes too.
func commitKeyChange(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return configsync.BumpAndCommit(ctx, tx, configsync.DomainAPIKeys, tenantID, configsync.Event(configsync.DomainEndUsers, tenantID))
}

// commitAccountChange commits a transaction that changed what the key map
// takes from an end-user account (status, quota, restrictions).
func commitAccountChange(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return configsync.CommitTx(ctx, tx, configsync.Event(configsync.DomainEndUsers, tenantID))
}

package usage

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// openRouterModelSyncTimeout bounds one tenant's sync. A run writes only the rows
// that change (see openrouter_model_sync_rows.go) and usually takes seconds; the
// margin is for a slow upstream or database, not for a run that does it all.
const openRouterModelSyncTimeout = 5 * time.Minute

// runDueOpenRouterModelSyncs runs the scheduled sync for every tenant whose
// interval has elapsed. Only the cluster leader runs it: the catalog is
// shared, so another node would repeat the same upstream fetch and rewrite
// the same rows. A single node is always the leader.
func runDueOpenRouterModelSyncs(ctx context.Context) {
	if !cluster.Default().IsLeader() {
		return
	}
	for _, tenantID := range enabledOpenRouterSyncTenantIDs() {
		state := GetOpenRouterModelSyncStateForTenant(tenantID)
		if !isOpenRouterModelSyncDue(state, time.Now().UTC()) {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, openRouterModelSyncTimeout)
		_, _, err := RunOpenRouterModelSyncForTenant(syncCtx, tenantID)
		cancel()
		if err != nil {
			log.Warnf("usage: scheduled openrouter model sync failed for tenant %s: %v", tenantID, err)
		}
	}
}

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// lockUsageRollupRebuildTx serialises the wipe-and-rebuild of
// usage_rollup_buckets across nodes and reports whether it still has to run.
// usageProjectionMu only covers this process; a rebuild that queued behind
// another node's catch-up must re-read the marker under the lock, or it would
// wipe a projection that is already done and whose detail rows retention may
// have pruned since.
func lockUsageRollupRebuildTx(tx *sql.Tx) (proceed bool, err error) {
	if err := cluster.XactLock(context.Background(), tx, cluster.LockUsageRollupRebuild); err != nil {
		return false, fmt.Errorf("usage: rollup backfill lock: %w", err)
	}
	var marker string
	err = tx.QueryRow(`SELECT marker_value FROM usage_projection_markers WHERE marker_key = ?`, usageRollupBackfillMarker).Scan(&marker)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("usage: rollup backfill marker under lock: %w", err)
	}
	return strings.TrimSpace(marker) != rollupMarkerDone, nil
}

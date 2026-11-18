package executor

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cluster/sharedstate"
)

// SharedIDMinter is the cluster-wide synthetic identifier service.
//
// The executor keeps the local cache implementations authoritative when the
// shared service is unavailable; this compatibility surface lets cluster
// runtime wiring install the service without coupling the executor package to
// the cluster runtime.
type SharedIDMinter interface {
	Available() bool
	MintID(ctx context.Context, namespace, key, candidate string, ttl time.Duration, sliding bool) (sharedstate.MintedID, error)
}

var sharedIDMinter SharedIDMinter

// SetSharedIDMinter installs the cluster-wide synthetic identifier service.
func SetSharedIDMinter(minter SharedIDMinter) {
	sharedIDMinter = minter
}

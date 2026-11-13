package cmd

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/accessfreeze"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// accessFreezeLoadTimeout bounds the first read of the standing freezes at
// start. A slow database must not hold the gateway back; the refresh loop
// picks the freezes up on its next pass.
const accessFreezeLoadTimeout = 10 * time.Second

// startAccessFreezes starts the per-provider access freezes (package
// accessfreeze) over the runtime database and installs them for the request
// path and the management API. The caller closes the returned service on
// shutdown so queued refusals are still recorded.
func startAccessFreezes(loc *time.Location) *accessfreeze.Service {
	service := accessfreeze.NewService(accessfreeze.NewStore(usage.RuntimeDB()), loc)
	ctx, cancel := context.WithTimeout(context.Background(), accessFreezeLoadTimeout)
	service.Start(ctx)
	cancel()
	accessfreeze.SetRuntime(service)
	return service
}

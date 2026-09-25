package cmd

import (
	"context"
	"sync"
	"time"

	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/clusterstate"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
)

var (
	sharedStateMu   sync.Mutex
	sharedStateStop func()
)

// startSharedSessionState moves the state a request may need on any node into
// the runtime database. Warmup policies are stored in every mode: in memory
// they vanished on restart. OAuth logins, video task routes and console job
// snapshots are shared only in cluster mode; a single node keeps them in
// memory as before.
//
// Nothing here subscribes to the cluster bus at startup: the OAuth store
// subscribes on first use, on whatever coordinator is installed by then.
func startSharedSessionState(cfg *config.Config) {
	db := usage.RuntimeDB()
	if db == nil {
		return
	}
	stopSharedSessionState()
	managementHandlers.SetWarmupPolicyStore(clusterstate.NewWarmupPolicies(db))
	if cfg == nil || !cfg.Cluster.Enabled {
		return
	}
	oauthStore := oauthsession.NewClusterStore(clusterstate.NewOAuthSessions(db), cluster.Default, oauthsession.DefaultTTL)
	managementHandlers.SetSharedOAuthSessions(oauthStore)
	managementHandlers.SetSharedJobSnapshots(clusterstate.NewManagementJobs(db))
	openaihandlers.SetVideoJobRouteStore(videoJobRoutes{routes: clusterstate.NewTaskRoutes(db)})
	stopJanitor := clusterstate.StartJanitor(clusterstate.JanitorInterval, nil, func(ctx context.Context) error {
		return clusterstate.Sweep(ctx, db)
	})

	sharedStateMu.Lock()
	sharedStateStop = func() {
		stopJanitor()
		openaihandlers.SetVideoJobRouteStore(nil)
		managementHandlers.SetSharedJobSnapshots(nil)
		managementHandlers.SetSharedOAuthSessions(nil)
		oauthStore.Close()
	}
	sharedStateMu.Unlock()
}

// stopSharedSessionState undoes startSharedSessionState.
func stopSharedSessionState() {
	sharedStateMu.Lock()
	stop := sharedStateStop
	sharedStateStop = nil
	sharedStateMu.Unlock()
	if stop != nil {
		stop()
	}
	managementHandlers.SetWarmupPolicyStore(nil)
}

// videoJobKind names video submissions in async_task_routes.
const videoJobKind = "video"

// videoJobRoutes adapts async_task_routes to the SDK's video route store.
type videoJobRoutes struct {
	routes *clusterstate.TaskRoutes
}

func (v videoJobRoutes) RememberVideoJob(ctx context.Context, requestID string, route openaihandlers.VideoJobRoute, ttl time.Duration) error {
	return v.routes.Remember(ctx, clusterstate.TaskRoute{
		Kind:     videoJobKind,
		TaskID:   requestID,
		Provider: route.Provider,
		AuthID:   route.AuthID,
		TenantID: route.TenantID,
		Model:    route.Model,
	}, ttl)
}

func (v videoJobRoutes) LookupVideoJob(ctx context.Context, requestID string) (openaihandlers.VideoJobRoute, bool, error) {
	route, ok, err := v.routes.Lookup(ctx, videoJobKind, requestID)
	if err != nil || !ok {
		return openaihandlers.VideoJobRoute{}, ok, err
	}
	return openaihandlers.VideoJobRoute{Model: route.Model, Provider: route.Provider, AuthID: route.AuthID, TenantID: route.TenantID}, true, nil
}

func (v videoJobRoutes) ForgetVideoJob(ctx context.Context, requestID string) error {
	return v.routes.Forget(ctx, videoJobKind, requestID)
}

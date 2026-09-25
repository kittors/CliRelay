package clusterstate

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	log "github.com/sirupsen/logrus"
)

// JanitorInterval is how often the leader expires shared session state.
const JanitorInterval = time.Minute

const sweepTimeout = 30 * time.Second

// Sweep expires and deletes shared session state past its window once:
// logins nobody can finish anymore, task routes and job snapshots past
// retention. Every statement is idempotent, so a sweep that runs twice during
// a leadership change does no harm.
func Sweep(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	return errors.Join(
		sweepOAuthSessions(ctx, db, oauthsession.OwnerLostAfter),
		sweepTaskRoutes(ctx, db),
		sweepManagementJobs(ctx, db),
	)
}

// StartJanitor runs sweep every interval while this node is the leader, and
// returns a function that stops it.
func StartJanitor(interval time.Duration, isLeader func() bool, sweep func(context.Context) error) (stop func()) {
	if interval <= 0 {
		interval = JanitorInterval
	}
	if isLeader == nil {
		isLeader = func() bool { return cluster.Default().IsLeader() }
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !isLeader() {
				continue
			}
			sweepCtx, sweepCancel := context.WithTimeout(ctx, sweepTimeout)
			if err := sweep(sweepCtx); err != nil && ctx.Err() == nil {
				log.WithError(err).Warn("clusterstate: shared session sweep failed")
			}
			sweepCancel()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

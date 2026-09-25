package openai

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Submission registry for asynchronous video jobs.
//
// A video request id belongs to the credential that created it: polling it with a
// different account returns 404. The proxy therefore has to remember which
// credential served each submission, down to the account, and pin every poll to
// it. Remembering only the provider was not enough once a tenant held more than
// one xAI account: a poll could land on a sibling account and get its 404.
//
// A single process keeps the registry in memory. The alternative — a table — buys
// durability across restarts for a job that xAI itself expires, and every id here
// is worthless within the hour. A restart during a generation costs the caller a
// clear "not known to this server" answer, which is honest and recoverable by
// resubmitting; it is not silent data loss. Several processes serving one
// deployment are another matter: the poll may reach a process that never saw the
// submission, so the embedding runtime installs a shared VideoJobRouteStore.
const videoJobTTL = time.Hour

// VideoJobRoute is what a poll needs to reach the account that created a video
// request.
type VideoJobRoute struct {
	Model    string
	Provider string
	AuthID   string
	TenantID string
}

// VideoJobRouteStore shares video submissions between the processes serving one
// deployment. The runtime installs one with SetVideoJobRouteStore when it runs
// as a cluster; without one the handlers use their in-process registry.
type VideoJobRouteStore interface {
	RememberVideoJob(ctx context.Context, requestID string, route VideoJobRoute, ttl time.Duration) error
	LookupVideoJob(ctx context.Context, requestID string) (VideoJobRoute, bool, error)
	ForgetVideoJob(ctx context.Context, requestID string) error
}

type videoJobRouteStoreHolder struct{ store VideoJobRouteStore }

var sharedVideoJobs atomic.Pointer[videoJobRouteStoreHolder]

// SetVideoJobRouteStore installs store for every video handler of this process;
// nil restores the in-process registry.
func SetVideoJobRouteStore(store VideoJobRouteStore) {
	if store == nil {
		sharedVideoJobs.Store(nil)
		return
	}
	sharedVideoJobs.Store(&videoJobRouteStoreHolder{store: store})
}

func sharedVideoJobStore() VideoJobRouteStore {
	if holder := sharedVideoJobs.Load(); holder != nil {
		return holder.store
	}
	return nil
}

type videoJob struct {
	VideoJobRoute

	expiresAt time.Time
}

var videoJobRegistry sync.Map

// rememberVideoJob records a submission. With a shared store the store is the
// only registry: an entry kept here too would be invisible to the other
// processes anyway.
func rememberVideoJob(ctx context.Context, requestID string, route VideoJobRoute) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}
	if store := sharedVideoJobStore(); store != nil {
		return store.RememberVideoJob(ctx, requestID, route, videoJobTTL)
	}
	videoJobRegistry.Store(requestID, videoJob{VideoJobRoute: route, expiresAt: time.Now().Add(videoJobTTL)})
	purgeExpiredVideoJobs()
	return nil
}

func lookupVideoJob(ctx context.Context, requestID string) (VideoJobRoute, bool, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return VideoJobRoute{}, false, nil
	}
	if store := sharedVideoJobStore(); store != nil {
		return store.LookupVideoJob(ctx, requestID)
	}
	raw, ok := videoJobRegistry.Load(requestID)
	if !ok {
		return VideoJobRoute{}, false, nil
	}
	job, _ := raw.(videoJob)
	if time.Now().After(job.expiresAt) {
		videoJobRegistry.Delete(requestID)
		return VideoJobRoute{}, false, nil
	}
	return job.VideoJobRoute, true, nil
}

func forgetVideoJob(ctx context.Context, requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil
	}
	if store := sharedVideoJobStore(); store != nil {
		return store.ForgetVideoJob(ctx, requestID)
	}
	videoJobRegistry.Delete(requestID)
	return nil
}

// purgeExpiredVideoJobs keeps the map from growing without bound on a long-running
// process whose callers never poll to completion.
func purgeExpiredVideoJobs() {
	now := time.Now()
	videoJobRegistry.Range(func(key, value any) bool {
		if job, ok := value.(videoJob); ok && now.After(job.expiresAt) {
			videoJobRegistry.Delete(key)
		}
		return true
	})
}

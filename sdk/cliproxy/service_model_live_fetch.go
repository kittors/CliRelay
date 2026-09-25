package cliproxy

import (
	"context"
	"errors"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/sdkbridge/service"
	log "github.com/sirupsen/logrus"
)

// Background refresh of self-listing credentials' model lists.
//
// Registration used to ask the upstream inline, so every registration waited on
// it. The startup pass did that for each credential in turn before the HTTP
// listener came up: on 2026-09-25 a node that could not reach the credentials'
// proxies spent more than 200s there, each call waiting out its timeout, and
// failed the deploy's 90s readiness check. Registrations after startup waited the
// same way, holding up the queue that applies credential changes.
//
// Registration now never waits on an upstream. It registers the credential's last
// known list (see credentialModelLists) and asks for a live one here. A bounded
// number of fetches run at a time, each under its own timeout. A live answer that
// differs from the last known list is saved and re-registers the credential; a
// failure keeps what the credential has registered and is retried with backoff.

const (
	// liveModelFetchConcurrency bounds the listing calls in flight, so a restart
	// with many credentials does not burst every upstream at once.
	liveModelFetchConcurrency = 4
	// liveModelFetchTimeout bounds one listing call, fallbacks between base URLs
	// included.
	liveModelFetchTimeout = 15 * time.Second
	// liveModelFetchFreshFor is how long a live answer satisfies further requests
	// for the same credential. Credential updates re-register often (a token
	// refresh is one), and each would otherwise ask the upstream again.
	liveModelFetchFreshFor = 5 * time.Minute
	// A failing credential is retried after liveModelFetchRetryMin, doubling up to
	// liveModelFetchRetryMax.
	liveModelFetchRetryMin = time.Minute
	liveModelFetchRetryMax = 15 * time.Minute
	// liveModelFetchSweepEvery is how often due retries are looked for.
	liveModelFetchSweepEvery = 30 * time.Second
)

var errLiveModelListEmpty = errors.New("model discovery: upstream listed no models")

type liveModelFetchFunc func(ctx context.Context, auth *coreauth.Auth, cfg *config.Config, provider string) ([]*ModelInfo, error)

type liveModelFetchStatus struct {
	provider    string
	inflight    bool
	lastSuccess time.Time
	failures    int
	retryAt     time.Time
}

// liveModelFetcher runs the background listing calls.
type liveModelFetcher struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	wg      sync.WaitGroup
	slots   chan struct{}
	status  map[string]*liveModelFetchStatus

	// Settings. Zero values take the defaults above when the fetcher starts.
	fetch       liveModelFetchFunc
	now         func() time.Time
	timeout     time.Duration
	concurrency int
	freshFor    time.Duration
	retryMin    time.Duration
	retryMax    time.Duration
	sweepEvery  time.Duration
}

func (st *liveModelFetcher) applyDefaults() {
	if st.fetch == nil {
		st.fetch = serviceapp.DiscoverCredentialModels
	}
	if st.now == nil {
		st.now = time.Now
	}
	if st.timeout <= 0 {
		st.timeout = liveModelFetchTimeout
	}
	if st.concurrency <= 0 {
		st.concurrency = liveModelFetchConcurrency
	}
	if st.freshFor <= 0 {
		st.freshFor = liveModelFetchFreshFor
	}
	if st.retryMin <= 0 {
		st.retryMin = liveModelFetchRetryMin
	}
	if st.retryMax < st.retryMin {
		st.retryMax = max(liveModelFetchRetryMax, st.retryMin)
	}
	if st.sweepEvery <= 0 {
		st.sweepEvery = liveModelFetchSweepEvery
	}
}

// selfListedModels returns the models a self-listing credential registers now,
// names where they came from, and asks for a live list in the background.
//
// The compiled-in floor is the last resort for a credential of a provider none of
// whose credentials ever answered: a gateway outage must not leave an otherwise
// working account with nothing routable.
func (s *Service) selfListedModels(a *coreauth.Auth, provider string, excluded []string) ([]*ModelInfo, string) {
	models, source := s.modelLists.lookup(a.ID, provider)
	if len(models) == 0 {
		models, source = serviceapp.CredentialModelFloor(provider), modelListSourceFloor
	}
	if a.Status != coreauth.StatusDisabled {
		s.requestLiveModelList(a.ID, provider)
	}
	return applyExcludedModels(models, excluded), source
}

// loadModelLists restores the lists the previous process saved. It runs before
// the startup registration pass, which registers from them.
func (s *Service) loadModelLists() {
	if s == nil {
		return
	}
	if s.modelLists.snapshotPath() == "" && s.cfg != nil {
		s.modelLists.setPath(serviceapp.ModelListSnapshotPath(s.cfg.AuthDir))
	}
	if loaded := s.modelLists.load(); loaded > 0 {
		log.Infof("model discovery: registering %d credentials from the model lists saved at %s", loaded, s.modelLists.snapshotPath())
	}
}

// saveModelLists writes the lists of the credentials that still exist.
func (s *Service) saveModelLists() {
	if s == nil || s.coreManager == nil {
		return
	}
	present := make(map[string]struct{})
	for _, auth := range s.coreManager.List() {
		if auth != nil {
			present[auth.ID] = struct{}{}
		}
	}
	keep := func(authID string) bool {
		_, ok := present[authID]
		return ok
	}
	if err := s.modelLists.persist(keep); err != nil {
		log.Warnf("model discovery: saving the model lists to %s failed: %v", s.modelLists.snapshotPath(), err)
	}
}

// startLiveModelLists starts the fetcher. It runs before the startup
// registration pass, so the first fetches overlap it.
func (s *Service) startLiveModelLists(parent context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	st := &s.liveModels
	st.mu.Lock()
	if st.ctx != nil {
		st.mu.Unlock()
		return
	}
	st.applyDefaults()
	ctx, cancel := context.WithCancel(parent)
	st.ctx, st.cancel = ctx, cancel
	st.slots = make(chan struct{}, st.concurrency)
	st.status = make(map[string]*liveModelFetchStatus)
	sweepEvery := st.sweepEvery
	st.wg.Add(1)
	st.mu.Unlock()

	go func() {
		defer st.wg.Done()
		ticker := time.NewTicker(sweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.retryDueLiveModelLists()
			}
		}
	}()
}

// stopLiveModelLists cancels every fetch and waits for them to return.
func (s *Service) stopLiveModelLists() {
	st := &s.liveModels
	st.mu.Lock()
	if st.ctx == nil || st.stopped {
		st.mu.Unlock()
		return
	}
	st.stopped = true
	st.cancel()
	st.mu.Unlock()
	st.wg.Wait()
}

// requestLiveModelList asks for a credential's live list. Nothing starts before
// the fetcher starts or after it stops, while a fetch for the credential is
// running, within freshFor of its last live answer, or before a failing
// credential's retry is due.
func (s *Service) requestLiveModelList(authID, provider string) {
	if s == nil || s.coreManager == nil || authID == "" {
		return
	}
	st := &s.liveModels
	st.mu.Lock()
	ctx := st.ctx
	if ctx == nil || st.stopped || ctx.Err() != nil || !st.admit(authID, provider) {
		st.mu.Unlock()
		return
	}
	st.wg.Add(1)
	st.mu.Unlock()

	go s.runLiveModelFetch(ctx, authID, provider)
}

// admit marks a credential's fetch as running when one is due. Callers hold mu.
func (st *liveModelFetcher) admit(authID, provider string) bool {
	status := st.status[authID]
	if status == nil {
		status = &liveModelFetchStatus{}
		st.status[authID] = status
	}
	now := st.now()
	switch {
	case status.inflight:
		return false
	case status.failures > 0 && now.Before(status.retryAt):
		return false
	case status.failures == 0 && !status.lastSuccess.IsZero() && now.Sub(status.lastSuccess) < st.freshFor:
		return false
	}
	status.inflight = true
	status.provider = provider
	return true
}

func (s *Service) runLiveModelFetch(ctx context.Context, authID, provider string) {
	st := &s.liveModels
	defer st.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("model discovery panicked: provider=%s auth=%s err=%v", provider, authID, recovered)
			st.release(authID)
		}
	}()

	select {
	case st.slots <- struct{}{}:
	case <-ctx.Done():
		st.release(authID)
		return
	}
	defer func() { <-st.slots }()

	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
		st.forget(authID)
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, st.timeout)
	models, err := st.fetch(fetchCtx, auth, s.configSnapshot(), provider)
	cancel()
	if ctx.Err() != nil {
		// Shutting down: the upstream did not fail, the fetch was cancelled.
		st.release(authID)
		return
	}
	if err == nil && len(models) == 0 {
		err = errLiveModelListEmpty
	}
	if err != nil {
		s.recordLiveModelListFailure(authID, provider, err)
		return
	}

	changed := s.modelLists.store(authID, provider, models, st.now())
	if failures := st.recordSuccess(authID); failures > 0 {
		log.Infof("model discovery: %s listing for auth %s answered again after %d failed attempts", provider, authID, failures)
	}
	if !changed {
		return
	}
	s.saveModelLists()
	if current, found := s.coreManager.GetByID(authID); found {
		s.registerModelsForAuth(ctx, current)
	}
}

func (s *Service) recordLiveModelListFailure(authID, provider string, err error) {
	st := &s.liveModels
	st.mu.Lock()
	status := st.status[authID]
	if status == nil {
		status = &liveModelFetchStatus{provider: provider}
		st.status[authID] = status
	}
	status.inflight = false
	status.failures++
	delay := st.backoff(status.failures)
	status.retryAt = st.now().Add(delay)
	failures := status.failures
	st.mu.Unlock()

	registered := len(GlobalModelRegistry().GetModelsForClient(authID))
	if failures == 1 {
		log.Warnf("model discovery: %s listing for auth %s failed: %v; keeping the %d models it has registered, retrying in %s", provider, authID, err, registered, delay)
		return
	}
	log.Debugf("model discovery: %s listing for auth %s failed again (%d attempts): %v; keeping the %d models it has registered, retrying in %s", provider, authID, failures, err, registered, delay)
}

// recordSuccess clears a credential's failures and returns how many there were.
func (st *liveModelFetcher) recordSuccess(authID string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	status := st.status[authID]
	if status == nil {
		return 0
	}
	failures := status.failures
	status.inflight = false
	status.failures = 0
	status.retryAt = time.Time{}
	status.lastSuccess = st.now()
	return failures
}

// release ends a fetch that neither succeeded nor failed.
func (st *liveModelFetcher) release(authID string) {
	st.mu.Lock()
	if status := st.status[authID]; status != nil {
		status.inflight = false
	}
	st.mu.Unlock()
}

// forget drops the state of a credential that no longer exists or is disabled.
func (st *liveModelFetcher) forget(authID string) {
	st.mu.Lock()
	delete(st.status, authID)
	st.mu.Unlock()
}

func (st *liveModelFetcher) backoff(failures int) time.Duration {
	delay := st.retryMin
	for i := 1; i < failures && delay < st.retryMax; i++ {
		delay *= 2
	}
	return min(delay, st.retryMax)
}

// retryDueLiveModelLists restarts the fetches of failing credentials whose retry
// is due.
func (s *Service) retryDueLiveModelLists() {
	st := &s.liveModels
	type due struct{ authID, provider string }
	var pending []due
	st.mu.Lock()
	now := st.now()
	for authID, status := range st.status {
		if status.inflight || status.failures == 0 || now.Before(status.retryAt) {
			continue
		}
		pending = append(pending, due{authID: authID, provider: status.provider})
	}
	st.mu.Unlock()
	for _, item := range pending {
		s.requestLiveModelList(item.authID, item.provider)
	}
}

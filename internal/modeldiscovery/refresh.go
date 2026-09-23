package modeldiscovery

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

// fetchTimeout bounds one upstream listing call.
const fetchTimeout = 15 * time.Second

// maxRefreshCandidates bounds how many credentials one background refresh tries. A
// listing endpoint that is down fails for every account alike, and walking a
// tenant's whole pool would only multiply the load on it.
const maxRefreshCandidates = 3

// Fetcher asks the upstream which models a credential can call.
type Fetcher func(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) ([]*sdkmodelcatalog.ModelInfo, error)

var fetchers = map[string]Fetcher{
	// Routing providers must use a fetch that reports failure as failure.
	"codex":  executor.DiscoverCodexModels,
	"claude": displayOnly(executor.FetchClaudeModels),
	"xai":    displayOnly(executor.FetchXAIModels),
	"kimi":   displayOnly(executor.FetchKimiModels),
}

var (
	errNoModels  = errors.New("model discovery: upstream listed no models")
	errNoFetcher = errors.New("model discovery: provider has no listing fetch")
)

// displayOnly adapts a panel-oriented fetch that answers with a cached list when the
// upstream call fails. Such an answer may not be live, which is acceptable only for
// providers that do not drive routing.
func displayOnly(fetch func(context.Context, *coreauth.Auth, *config.Config) []*sdkmodelcatalog.ModelInfo) Fetcher {
	return func(ctx context.Context, auth *coreauth.Auth, cfg *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		models := fetch(ctx, auth, cfg)
		if len(models) == 0 {
			return nil, errNoModels
		}
		return models, nil
	}
}

// SetFetcherForTest replaces a provider's fetch and returns a func restoring it.
func SetFetcherForTest(provider string, fetch Fetcher) func() {
	provider = NormalizeProvider(provider)
	mu.Lock()
	previous, existed := fetchers[provider]
	fetchers[provider] = fetch
	mu.Unlock()
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if existed {
			fetchers[provider] = previous
			return
		}
		delete(fetchers, provider)
	}
}

// Warm fetches a provider's list with one credential and stores it.
//
// Concurrent callers for the same tenant and provider share one upstream call.
// Without force a fresh stored list is returned as is, and waiters on a failed
// shared call get the failure instead of each retrying it. With force the stored
// list is bypassed, and a failed call made by someone else is tried once more.
func Warm(ctx context.Context, auth *coreauth.Auth, cfg *config.Config, tenantID, provider string, force bool) ([]*sdkmodelcatalog.ModelInfo, bool) {
	provider = NormalizeProvider(provider)
	if auth == nil || !IsShared(provider) || !IsDiscoveryCredential(auth, provider) {
		return nil, false
	}
	key := storeKey(tenantID, provider)

	mu.Lock()
	if !force {
		if stored, ok := entries[key]; ok && now().Sub(stored.fetchedAt) <= FreshFor {
			models := cloneModels(stored.models)
			mu.Unlock()
			return models, true
		}
	}
	if pending, ok := inflight[key]; ok {
		mu.Unlock()
		<-pending.done
		if pending.ok || !force {
			return cloneModels(pending.models), pending.ok
		}
		mu.Lock()
		if again, ok := inflight[key]; ok {
			mu.Unlock()
			<-again.done
			return cloneModels(again.models), again.ok
		}
	}
	leader := &flight{done: make(chan struct{})}
	inflight[key] = leader
	fetch := fetchers[provider]
	mu.Unlock()

	models, err := runFetch(ctx, fetch, auth, cfg)
	ok := err == nil && len(models) > 0
	if ok {
		Store(tenantID, provider, models)
	} else {
		log.Debugf("model discovery: %s listing via %s failed: %v", provider, auth.ID, err)
	}

	mu.Lock()
	if ok {
		leader.models = cloneModels(models)
	}
	leader.ok = ok
	delete(inflight, key)
	close(leader.done)
	mu.Unlock()

	if !ok {
		return nil, false
	}
	return cloneModels(models), true
}

func runFetch(ctx context.Context, fetch Fetcher, auth *coreauth.Auth, cfg *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
	if fetch == nil {
		return nil, errNoFetcher
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	models, err := fetch(fetchCtx, auth, cfg)
	if err == nil && len(models) == 0 {
		err = errNoModels
	}
	return models, err
}

// Candidates lists the credentials that may fetch a provider's list for a tenant:
// enabled, eligible per IsDiscoveryCredential, healthy ones first, in a stable order.
func Candidates(manager *coreauth.Manager, tenantID, provider string) []*coreauth.Auth {
	if manager == nil {
		return nil
	}
	provider = NormalizeProvider(provider)
	var healthy, degraded []*coreauth.Auth
	for _, auth := range manager.ListForTenant(tenantID) {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		if NormalizeProvider(auth.Provider) != provider || !IsDiscoveryCredential(auth, provider) {
			continue
		}
		if auth.Unavailable {
			degraded = append(degraded, auth)
			continue
		}
		healthy = append(healthy, auth)
	}
	sortByID(healthy)
	sortByID(degraded)
	return append(healthy, degraded...)
}

func sortByID(auths []*coreauth.Auth) {
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
}

// Ensure returns a provider's list for a tenant, fetching it with the tenant's first
// eligible credential when nothing fresh is stored or force is set. If that fetch
// fails, the last stored list is returned however old it is.
//
// Panels call this on page load, so it tries a single credential; trying more is the
// background refresh's job.
func Ensure(ctx context.Context, manager *coreauth.Manager, cfg *config.Config, tenantID, provider string, force bool) []*sdkmodelcatalog.ModelInfo {
	provider = NormalizeProvider(provider)
	if !IsShared(provider) {
		return nil
	}
	if !force {
		if fresh := Fresh(tenantID, provider); len(fresh) > 0 {
			return fresh
		}
	}
	if candidates := Candidates(manager, tenantID, provider); len(candidates) > 0 {
		if live, ok := Warm(ctx, candidates[0], cfg, tenantID, provider, force); ok {
			return live
		}
	}
	return Snapshot(tenantID, provider)
}

// EnsureForTenant runs Ensure for every shared provider that has an eligible
// credential in the tenant, keyed by provider.
func EnsureForTenant(ctx context.Context, manager *coreauth.Manager, cfg *config.Config, tenantID string, force bool) map[string][]*sdkmodelcatalog.ModelInfo {
	out := make(map[string][]*sdkmodelcatalog.ModelInfo)
	if manager == nil {
		return out
	}
	seen := make(map[string]struct{})
	for _, auth := range manager.ListForTenant(tenantID) {
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		provider := NormalizeProvider(auth.Provider)
		if !IsShared(provider) || !IsDiscoveryCredential(auth, provider) {
			continue
		}
		if _, done := seen[provider]; done {
			continue
		}
		seen[provider] = struct{}{}
		if models := Ensure(ctx, manager, cfg, tenantID, provider, force); len(models) > 0 {
			out[provider] = models
		}
	}
	return out
}

// Refresh re-fetches a provider's list for a tenant, trying up to
// maxRefreshCandidates credentials and stopping at the first that answers. It
// reports whether a live list was stored.
func Refresh(ctx context.Context, manager *coreauth.Manager, cfg *config.Config, tenantID, provider string) bool {
	candidates := Candidates(manager, tenantID, provider)
	if len(candidates) > maxRefreshCandidates {
		candidates = candidates[:maxRefreshCandidates]
	}
	for _, auth := range candidates {
		if ctx != nil && ctx.Err() != nil {
			return false
		}
		if _, ok := Warm(ctx, auth, cfg, tenantID, provider, true); ok {
			return true
		}
	}
	return false
}

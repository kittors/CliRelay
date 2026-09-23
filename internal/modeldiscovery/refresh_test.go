package modeldiscovery

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

var errFetchFailed = errors.New("listing unavailable")

func useFetcher(t *testing.T, provider string, fetch Fetcher) {
	t.Helper()
	t.Cleanup(SetFetcherForTest(provider, fetch))
}

func failingFetch(context.Context, *coreauth.Auth, *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
	return nil, errFetchFailed
}

func registerAuths(t *testing.T, auths ...*coreauth.Auth) *coreauth.Manager {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", auth.ID, err)
		}
	}
	return manager
}

func codexAuth(id, tenantID string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		TenantID: tenantID,
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": id + "@example.com"},
	}
}

func authIDs(auths []*coreauth.Auth) []string {
	out := make([]string, 0, len(auths))
	for _, auth := range auths {
		out = append(out, auth.ID)
	}
	return out
}

func TestWarmFailureKeepsTheLastKnownList(t *testing.T) {
	resetStore(t)
	Store("tenant-a", "codex", models("gpt-a"))
	useFetcher(t, "codex", failingFetch)

	if _, ok := Warm(context.Background(), codexAuth("codex-a", "tenant-a"), nil, "tenant-a", "codex", true); ok {
		t.Fatal("Warm reported success for a failed fetch")
	}
	if got := ids(Snapshot("tenant-a", "codex")); !slices.Equal(got, []string{"gpt-a"}) {
		t.Fatalf("Snapshot = %v, a failed fetch must keep the last known list", got)
	}
}

func TestWarmSharesOneUpstreamCall(t *testing.T) {
	resetStore(t)
	var calls atomic.Int32
	release := make(chan struct{})
	useFetcher(t, "codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		calls.Add(1)
		<-release
		return models("gpt-a"), nil
	})
	auth := codexAuth("codex-a", "tenant-a")

	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = Warm(context.Background(), auth, nil, "tenant-a", "codex", false)
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	// Callers that arrived during the fetch waited for it; later ones found it stored.
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	for i, ok := range results {
		if !ok {
			t.Fatalf("caller %d did not get the shared answer", i)
		}
	}
}

func TestWarmRefusesAnIneligibleCredential(t *testing.T) {
	resetStore(t)
	var calls atomic.Int32
	useFetcher(t, "codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		calls.Add(1)
		return models("relay-model"), nil
	})
	apiKey := &coreauth.Auth{ID: "codex-key", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}

	if _, ok := Warm(context.Background(), apiKey, nil, "tenant-a", "codex", true); ok || calls.Load() != 0 {
		t.Fatalf("API key warmed the shared list (ok=%v, calls=%d)", ok, calls.Load())
	}
	if snapshot := Snapshot("tenant-a", "codex"); len(snapshot) != 0 {
		t.Fatalf("Snapshot = %v, want nothing stored", ids(snapshot))
	}
}

func TestRefreshMovesPastAFailingCredential(t *testing.T) {
	resetStore(t)
	manager := registerAuths(t, codexAuth("codex-b", "tenant-a"), codexAuth("codex-a", "tenant-a"))
	var (
		triedMu sync.Mutex
		tried   []string
	)
	useFetcher(t, "codex", func(_ context.Context, auth *coreauth.Auth, _ *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		triedMu.Lock()
		tried = append(tried, auth.ID)
		triedMu.Unlock()
		if auth.ID == "codex-a" {
			return nil, errors.New("token expired")
		}
		return models("gpt-from-b"), nil
	})

	if !Refresh(context.Background(), manager, nil, "tenant-a", "codex") {
		t.Fatal("Refresh gave up while a working credential remained")
	}
	if !slices.Equal(tried, []string{"codex-a", "codex-b"}) {
		t.Fatalf("tried = %v, want a stable order ending at the working credential", tried)
	}
	if got := ids(Snapshot("tenant-a", "codex")); !slices.Equal(got, []string{"gpt-from-b"}) {
		t.Fatalf("Snapshot = %v", got)
	}
}

func TestRefreshGivesUpAfterMaxCandidates(t *testing.T) {
	resetStore(t)
	manager := registerAuths(t,
		codexAuth("codex-1", "tenant-a"), codexAuth("codex-2", "tenant-a"), codexAuth("codex-3", "tenant-a"),
		codexAuth("codex-4", "tenant-a"), codexAuth("codex-5", "tenant-a"),
	)
	var calls atomic.Int32
	useFetcher(t, "codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		calls.Add(1)
		return nil, errFetchFailed
	})

	if Refresh(context.Background(), manager, nil, "tenant-a", "codex") {
		t.Fatal("Refresh reported success with every fetch failing")
	}
	if got := calls.Load(); got != maxRefreshCandidates {
		t.Fatalf("fetches = %d, want %d: a listing outage fails for every account alike", got, maxRefreshCandidates)
	}
}

func TestCandidatesPreferHealthyOAuthCredentials(t *testing.T) {
	degraded := codexAuth("codex-c", "tenant-a")
	degraded.Unavailable = true
	disabled := codexAuth("codex-off", "tenant-a")
	disabled.Disabled = true
	manager := registerAuths(t,
		degraded,
		codexAuth("codex-b", "tenant-a"),
		codexAuth("codex-a", "tenant-a"),
		&coreauth.Auth{ID: "codex-key", Provider: "codex", TenantID: "tenant-a", Attributes: map[string]string{"api_key": "sk-test"}},
		disabled,
		codexAuth("codex-elsewhere", "tenant-b"),
		&coreauth.Auth{ID: "xai-a", Provider: "xai", TenantID: "tenant-a"},
	)

	got := authIDs(Candidates(manager, "tenant-a", "codex"))

	if !slices.Equal(got, []string{"codex-a", "codex-b", "codex-c"}) {
		t.Fatalf("candidates = %v", got)
	}
}

func TestEnsureFallsBackToTheSnapshot(t *testing.T) {
	resetStore(t)
	clock := useClock(t)
	Store("tenant-a", "codex", models("gpt-old"))
	clock.advance(FreshFor + time.Hour)
	manager := registerAuths(t, codexAuth("codex-a", "tenant-a"))
	useFetcher(t, "codex", failingFetch)

	got := ids(Ensure(context.Background(), manager, nil, "tenant-a", "codex", false))

	if !slices.Equal(got, []string{"gpt-old"}) {
		t.Fatalf("Ensure = %v, want the stale list rather than nothing", got)
	}
}

func TestEnsureForTenantIgnoresCodexAPIKeys(t *testing.T) {
	resetStore(t)
	var calls atomic.Int32
	useFetcher(t, "codex", func(context.Context, *coreauth.Auth, *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
		calls.Add(1)
		return models("relay-model"), nil
	})
	manager := registerAuths(t, &coreauth.Auth{
		ID:         "codex-key",
		Provider:   "codex",
		TenantID:   "tenant-a",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": "https://relay.example.com/v1"},
	})

	lists := EnsureForTenant(context.Background(), manager, nil, "tenant-a", false)

	if len(lists) != 0 || calls.Load() != 0 {
		t.Fatalf("lists = %v, calls = %d; a relay's catalog must not become the tenant's Codex list", lists, calls.Load())
	}
}

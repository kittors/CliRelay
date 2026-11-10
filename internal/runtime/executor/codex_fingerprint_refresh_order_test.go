package executor

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCodexFingerprintRefreshCannotReplaceUnpersistedLearning(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	const learnedUA = "codex_cli_rs/0.153.4 (Mac OS 26.3.1; arm64) learned"
	cfg := &config.Config{IdentityFingerprint: config.IdentityFingerprintConfig{
		Codex: config.CodexIdentityFingerprintConfig{Enabled: true},
	}}
	auth := &cliproxyauth.Auth{ID: "refresh-order", Provider: "codex", Metadata: map[string]any{
		"access_token": "test-token", "account_id": "refresh-order-account",
	}}
	accountKey := authSubjectKey(t, auth)
	persistRelease, refreshRelease := make(chan struct{}), make(chan struct{})
	refreshStarted := make(chan struct{})
	var persistOnce, refreshOnce, startedOnce sync.Once
	var persisted atomic.Bool
	var reads atomic.Int32
	stored := refreshOrderProfile(accountKey, "codex_cli_rs", "0.153.4")
	stored.Fields[identityfingerprint.FieldUserAgent] = learnedUA
	defer func() {
		persistOnce.Do(func() { close(persistRelease) })
		refreshOnce.Do(func() { close(refreshRelease) })
		eventually(t, time.Second, func() bool {
			runtimeIdentityFingerprintAsync.Lock()
			defer runtimeIdentityFingerprintAsync.Unlock()
			return len(runtimeIdentityFingerprintAsync.persists) == 0
		})
		waitCodexFingerprintRefreshIdle(t, accountKey)
	}()
	runtimeIdentityFingerprintStoreFuncMu.Lock()
	runtimeObserveIdentityFingerprint = func(identityfingerprint.LearnInput) (*identityfingerprint.LearnedRecord, identityfingerprint.MergeResult, error) {
		<-persistRelease
		persisted.Store(true)
		return &stored, identityfingerprint.MergeResult{}, nil
	}
	runtimeListCodexIdentityFingerprintProfiles = func(identityfingerprint.Provider, string) ([]identityfingerprint.LearnedRecord, error) {
		reads.Add(1)
		startedOnce.Do(func() { close(refreshStarted) })
		<-refreshRelease
		if persisted.Load() {
			return []identityfingerprint.LearnedRecord{stored}, nil
		}
		return nil, nil // Snapshot read before the first learned profile is persisted.
	}
	runtimeGetCodexIdentityFingerprintAccountPolicy = func(identityfingerprint.Provider, string) (identityfingerprint.AccountPolicy, error) {
		return identityfingerprint.AccountPolicy{}, nil
	}
	runtimeIdentityFingerprintStoreFuncMu.Unlock()
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/responses", http.Header{
		"User-Agent": {learnedUA}, "Version": {"0.153.4"}, "Originator": {"codex_cli_rs"},
	})
	first := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil).WithContext(ctx)
	applyCodexHeaders(first, cfg, auth, "test-token", false)
	if got := first.Header.Get("User-Agent"); got != "codex_cli_rs/0.159.1 (Mac OS 26.3.1; arm64) learned" {
		t.Fatalf("first request UA=%q, want the current client with the learned suffix", got)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("background snapshot did not start")
	}
	refreshOnce.Do(func() { close(refreshRelease) })
	waitCodexFingerprintRefreshIdle(t, accountKey)
	replay := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	applyCodexHeaders(replay, cfg, auth, "test-token", false)
	if got := replay.Header.Get("User-Agent"); got != "codex_cli_rs/0.159.1 (Mac OS 26.3.1; arm64) learned" {
		t.Fatalf("old empty snapshot replaced learned UA: got %q", got)
	}
	persistOnce.Do(func() { close(persistRelease) })
	eventually(t, time.Second, func() bool {
		return reads.Load() >= 2
	})
	waitCodexFingerprintRefreshIdle(t, accountKey)
	selection, ok := getCachedCodexIdentityFingerprintSelection(accountKey)
	if !ok || selection.Profile == nil || selection.Profile.Fields[identityfingerprint.FieldUserAgent] != learnedUA {
		t.Fatalf("learning completion did not refresh the persisted selection: %+v", selection)
	}
}

func waitCodexFingerprintRefreshIdle(t *testing.T, accountKey string) {
	t.Helper()
	eventually(t, time.Second, func() bool {
		codexIdentityFingerprintSelectionCache.Lock()
		defer codexIdentityFingerprintSelectionCache.Unlock()
		_, refreshing := codexIdentityFingerprintSelectionCache.refreshing[accountKey]
		return !refreshing
	})
}

func TestCodexFingerprintInvalidationDuringRefreshMakesProgress(t *testing.T) {
	for _, deletion := range []bool{false, true} {
		name := "active policy"
		if deletion {
			name = "deleted profiles"
		}
		t.Run(name, func(t *testing.T) {
			initIdentityFingerprintRuntimeDB(t)
			const accountKey = "refresh-invalidated-account"
			old := refreshOrderProfile(accountKey, "codex_cli_rs", "0.150.0")
			next := refreshOrderProfile(accountKey, "codex_vscode", "0.153.4")
			next.ProfileFamily = identityfingerprint.ProfileFamilyIDE
			setCachedCodexIdentityFingerprintSelection(accountKey, identityfingerprint.ProfileSelection{Profile: &old})
			// A cached profile must not resurrect after the latest store snapshot
			// deletes it. The cache is deliberately left populated in this test.
			setCachedRuntimeIdentityFingerprint(identityfingerprint.ProviderCodex, accountKey, old.ProfileKey, "", &old, time.Minute)
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			var lists atomic.Int32
			defer func() {
				releaseOnce.Do(func() { close(release) })
				waitCodexFingerprintRefreshIdle(t, accountKey)
			}()
			runtimeIdentityFingerprintStoreFuncMu.Lock()
			runtimeListCodexIdentityFingerprintProfiles = func(identityfingerprint.Provider, string) ([]identityfingerprint.LearnedRecord, error) {
				if lists.Add(1) == 1 {
					close(started)
					<-release
					return []identityfingerprint.LearnedRecord{old}, nil
				}
				if deletion {
					return nil, nil
				}
				return []identityfingerprint.LearnedRecord{old, next}, nil
			}
			runtimeGetCodexIdentityFingerprintAccountPolicy = func(identityfingerprint.Provider, string) (identityfingerprint.AccountPolicy, error) {
				return identityfingerprint.AccountPolicy{AccountKey: accountKey, Strategy: identityfingerprint.AccountStrategyActiveProfile, ActiveProfileKey: next.ProfileKey, Revision: 2}, nil
			}
			runtimeIdentityFingerprintStoreFuncMu.Unlock()
			scheduleCodexIdentityFingerprintSelectionRefresh(accountKey)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("old refresh did not start")
			}
			invalidateCachedCodexIdentityFingerprintSelection(accountKey)
			releaseOnce.Do(func() { close(release) })
			eventually(t, time.Second, func() bool {
				selection, ok := getCachedCodexIdentityFingerprintSelection(accountKey)
				if !ok || selection.Policy.Revision != 2 {
					return false
				}
				if deletion {
					return selection.Profile == nil && selection.Reason == "no_profile"
				}
				return selection.Profile != nil && selection.Profile.ProfileKey == next.ProfileKey && selection.Reason == "active_profile"
			})
			if lists.Load() < 2 {
				t.Fatal("invalidation was lost instead of refreshing the latest snapshot")
			}
		})
	}
}

func refreshOrderProfile(accountKey, product, version string) identityfingerprint.LearnedRecord {
	return identityfingerprint.LearnedRecord{
		Provider: identityfingerprint.ProviderCodex, AccountKey: accountKey,
		ProfileKey: product, ProfileFamily: identityfingerprint.ProfileFamilyCLI,
		ClientProduct: product, ClientVariant: product, Version: version,
		Fields: map[string]string{
			identityfingerprint.FieldUserAgent:       product + "/" + version,
			identityfingerprint.FieldCodexVersion:    version,
			identityfingerprint.FieldCodexOriginator: product,
		},
		LastSeenAt: time.Now().UTC(),
	}
}

func TestCodexFingerprintDisabledSkipsLearningAndSelection(t *testing.T) {
	initIdentityFingerprintRuntimeDB(t)
	var calls atomic.Int32
	runtimeIdentityFingerprintStoreFuncMu.Lock()
	runtimeObserveIdentityFingerprint = func(identityfingerprint.LearnInput) (*identityfingerprint.LearnedRecord, identityfingerprint.MergeResult, error) {
		calls.Add(1)
		return nil, identityfingerprint.MergeResult{}, nil
	}
	runtimeListCodexIdentityFingerprintProfiles = func(identityfingerprint.Provider, string) ([]identityfingerprint.LearnedRecord, error) {
		calls.Add(1)
		return nil, nil
	}
	runtimeIdentityFingerprintStoreFuncMu.Unlock()
	ctx := contextWithInboundHeaders(http.MethodPost, "/v1/responses", http.Header{"User-Agent": {"codex_cli_rs/0.153.4"}})
	_, enabled := codexIdentityFingerprint(&config.Config{}, &cliproxyauth.Auth{Provider: "codex", ID: "disabled"}, ctx)
	if enabled || calls.Load() != 0 {
		t.Fatalf("disabled fingerprint enabled=%v store calls=%d", enabled, calls.Load())
	}
	codexIdentityFingerprintSelectionCache.Lock()
	defer codexIdentityFingerprintSelectionCache.Unlock()
	if len(codexIdentityFingerprintSelectionCache.refreshing) != 0 {
		t.Fatal("disabled fingerprint started a refresh")
	}
}

func resetIdentityFingerprintRuntimeStateForTest() {
	runtimeIdentityFingerprintCache.Lock()
	runtimeIdentityFingerprintCache.records = map[string]identityFingerprintCacheEntry{}
	runtimeIdentityFingerprintCache.Unlock()

	runtimeIdentityFingerprintAsync.Lock()
	runtimeIdentityFingerprintAsync.loads = map[string]struct{}{}
	runtimeIdentityFingerprintAsync.persists = map[string]struct{}{}
	runtimeIdentityFingerprintAsync.Unlock()
	resetRuntimeIdentityFingerprintColdLoaded()

	codexIdentityFingerprintSelectionCache.Lock()
	codexIdentityFingerprintSelectionCache.entries = map[string]codexIdentityFingerprintSelectionEntry{}
	codexIdentityFingerprintSelectionCache.refreshing = map[string]struct{}{}
	codexIdentityFingerprintSelectionCache.states = map[string]*codexIdentityFingerprintRefreshState{}
	codexIdentityFingerprintSelectionCache.Unlock()

	runtimeIdentityFingerprintStoreFuncMu.Lock()
	runtimeGetIdentityFingerprint = usage.GetIdentityFingerprint
	runtimeObserveIdentityFingerprint = usage.ObserveIdentityFingerprint
	runtimeListCodexIdentityFingerprintProfiles = usage.ListIdentityFingerprintProfiles
	runtimeGetCodexIdentityFingerprintAccountPolicy = usage.GetIdentityFingerprintAccountPolicy
	runtimeIdentityFingerprintStoreFuncMu.Unlock()
}

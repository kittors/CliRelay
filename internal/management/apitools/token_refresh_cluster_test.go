package apitools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type countingClaudeRefresher struct{ calls *atomic.Int32 }

func (r countingClaudeRefresher) RefreshTokens(context.Context, string) (*claudeauth.ClaudeTokenData, error) {
	r.calls.Add(1)
	return &claudeauth.ClaudeTokenData{AccessToken: "at-new", RefreshToken: "rt-new", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
}

// leaseStore is a versioned store that is also the refresh coordinator, the
// shape of the cluster store.
type leaseStore struct {
	mu       sync.Mutex
	latest   *coreauth.Auth
	acquired bool
	saved    []*coreauth.Auth
	releases int
}

func (s *leaseStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (s *leaseStore) Delete(context.Context, string) error           { return nil }

func (s *leaseStore) Save(_ context.Context, auth *coreauth.Auth) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if coreauth.CredentialVersion(auth) != coreauth.CredentialVersion(s.latest) {
		return "", coreauth.ErrCredentialConflict
	}
	next := auth.Clone()
	next.Metadata[coreauth.CredentialVersionMetadataKey] = float64(coreauth.CredentialVersion(s.latest) + 1)
	s.saved = append(s.saved, next)
	s.latest = next
	return "", nil
}

func (s *leaseStore) Get(context.Context, string) (*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest.Clone(), nil
}

func (s *leaseStore) ClaimRefresh(context.Context, string) (*coreauth.Auth, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.acquired {
		return nil, false, nil
	}
	return s.latest.Clone(), true, nil
}

func (s *leaseStore) ReleaseRefresh(context.Context, string) {
	s.mu.Lock()
	s.releases++
	s.mu.Unlock()
}

func claudeCredential(version float64, accessToken string, expires time.Time) *coreauth.Auth {
	return &coreauth.Auth{ID: "claude.json", Provider: "claude", Metadata: map[string]any{
		"type": "claude", "access_token": accessToken, "refresh_token": "rt-old",
		"expired": expires.Format(time.RFC3339), coreauth.CredentialVersionMetadataKey: version,
	}}
}

func newLeaseService(t *testing.T, store coreauth.Store, calls *atomic.Int32) (*Service, *coreauth.Manager) {
	t.Helper()
	mgr := coreauth.NewManager(store, nil, nil)
	svc := New(&config.Config{}, mgr, Dependencies{NewClaudeOAuthRefresher: func(*config.Config) ClaudeOAuthRefresher {
		return countingClaudeRefresher{calls: calls}
	}})
	return svc, mgr
}

func TestResolveTokenAdoptsRefreshDoneByAnotherNode(t *testing.T) {
	var calls atomic.Int32
	expired := claudeCredential(1, "at-expired", time.Now().Add(-time.Minute))
	store := &leaseStore{acquired: true, latest: claudeCredential(2, "at-fresh", time.Now().Add(time.Hour))}
	svc, mgr := newLeaseService(t, store, &calls)
	if _, err := mgr.Register(coreauth.WithSkipPersist(context.Background()), expired.Clone()); err != nil {
		t.Fatal(err)
	}
	token, err := svc.ResolveTokenForAuth(context.Background(), expired.Clone())
	if err != nil || token != "at-fresh" {
		t.Fatalf("token = %q, err = %v; want the token the other node refreshed", token, err)
	}
	if calls.Load() != 0 || len(store.saved) != 0 {
		t.Fatalf("refreshes=%d saves=%d, a refreshed copy must be adopted, not refreshed again", calls.Load(), len(store.saved))
	}
	if store.releases != 1 {
		t.Fatalf("releases = %d, want the lease released", store.releases)
	}
}

func TestResolveTokenRefreshesUnderLeaseAndPersistsOnNewestVersion(t *testing.T) {
	var calls atomic.Int32
	expired := claudeCredential(3, "at-expired", time.Now().Add(-time.Minute))
	store := &leaseStore{acquired: true, latest: expired.Clone()}
	svc, mgr := newLeaseService(t, store, &calls)
	if _, err := mgr.Register(coreauth.WithSkipPersist(context.Background()), expired.Clone()); err != nil {
		t.Fatal(err)
	}
	token, err := svc.ResolveTokenForAuth(context.Background(), expired.Clone())
	if err != nil || token != "at-new" || calls.Load() != 1 {
		t.Fatalf("token=%q err=%v refreshes=%d", token, err, calls.Load())
	}
	if len(store.saved) != 1 || store.saved[0].Metadata["refresh_token"] != "rt-new" {
		t.Fatalf("saved = %v, want the rotated tokens persisted once", store.saved)
	}
}

func TestResolveTokenGivesUpWhileAnotherNodeRefreshes(t *testing.T) {
	previous := refreshLeaseWait
	refreshLeaseWait = 300 * time.Millisecond
	t.Cleanup(func() { refreshLeaseWait = previous })
	var calls atomic.Int32
	expired := claudeCredential(1, "at-expired", time.Now().Add(-time.Minute))
	svc, _ := newLeaseService(t, &leaseStore{acquired: false, latest: expired.Clone()}, &calls)
	if _, err := svc.ResolveTokenForAuth(context.Background(), expired.Clone()); !errors.Is(err, coreauth.ErrRefreshInProgress) || calls.Load() != 0 {
		t.Fatalf("err = %v refreshes = %d, want ErrRefreshInProgress without refreshing", err, calls.Load())
	}
}

func TestResolveTokenWithoutCoordinatorRefreshesDirectly(t *testing.T) {
	var calls atomic.Int32
	svc, _ := newLeaseService(t, nil, &calls)
	expired := claudeCredential(0, "at-expired", time.Now().Add(-time.Minute))
	delete(expired.Metadata, coreauth.CredentialVersionMetadataKey)
	token, err := svc.ResolveTokenForAuth(context.Background(), expired)
	if err != nil || token != "at-new" || calls.Load() != 1 {
		t.Fatalf("single-node refresh: token=%q err=%v refreshes=%d", token, err, calls.Load())
	}
}

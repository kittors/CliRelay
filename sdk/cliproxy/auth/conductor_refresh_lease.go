package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type refreshCoordinatorHolder struct {
	coordinator RefreshCoordinator
}

// SetRefreshCoordinator installs the cluster refresh coordinator. nil keeps
// the single-node behaviour, where every process refreshes on its own.
func (m *Manager) SetRefreshCoordinator(coordinator RefreshCoordinator) {
	if m == nil {
		return
	}
	m.refreshCoordinator.Store(refreshCoordinatorHolder{coordinator: coordinator})
}

// adoptStoreRefreshCoordinator installs the store as refresh coordinator when
// it is one (the cluster store), and clears a coordinator a previous store
// provided. The file store is not one, so single-node managers never
// coordinate.
func (m *Manager) adoptStoreRefreshCoordinator(store Store) {
	coordinator, _ := store.(RefreshCoordinator)
	m.SetRefreshCoordinator(coordinator)
}

func (m *Manager) currentRefreshCoordinator() RefreshCoordinator {
	if m == nil {
		return nil
	}
	holder, _ := m.refreshCoordinator.Load().(refreshCoordinatorHolder)
	return holder.coordinator
}

// RefreshCoordinated reports whether credential refreshes take a cluster lease.
func (m *Manager) RefreshCoordinated() bool {
	return m.currentRefreshCoordinator() != nil
}

// RefreshLease is a held cluster refresh lease. Release it once the refreshed
// credential is persisted (or the refresh failed).
type RefreshLease struct {
	// Latest is the newest persisted copy of the credential when the lease
	// was taken, or nil when unknown.
	Latest *Auth

	release func()
	once    sync.Once
}

// Release gives the lease back. It is safe to call more than once.
func (l *RefreshLease) Release() {
	if l == nil || l.release == nil {
		return
	}
	l.once.Do(l.release)
}

const refreshLeasePollInterval = 250 * time.Millisecond

// AcquireRefreshLease claims the refresh lease of id, polling for up to wait
// while another node holds it. Without a coordinator it returns an empty lease
// at once, so single-node callers need no special case.
func (m *Manager) AcquireRefreshLease(ctx context.Context, id string, wait time.Duration) (*RefreshLease, error) {
	coordinator := m.currentRefreshCoordinator()
	if coordinator == nil {
		return &RefreshLease{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(wait)
	for {
		latest, acquired, err := coordinator.ClaimRefresh(ctx, id)
		if err != nil {
			return nil, err
		}
		if acquired {
			return &RefreshLease{Latest: latest, release: func() {
				coordinator.ReleaseRefresh(context.Background(), id)
			}}, nil
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			return nil, ErrRefreshInProgress
		}
		if !sleepContext(ctx, refreshLeasePollInterval) {
			return nil, ctx.Err()
		}
	}
}

// refreshClaim is the outcome of taking the lease before a background refresh.
type refreshClaim struct {
	proceed bool
	auth    *Auth
	release func()
}

func (c refreshClaim) done() {
	if c.release != nil {
		c.release()
	}
}

// claimBackgroundRefresh takes the cluster lease before the refresh loop
// spends a refresh token. Without a coordinator it always proceeds with auth.
// Otherwise it skips the round when another node holds the lease (that node
// publishes the result) or when the store cannot be reached: a rotated token
// that could not be persisted would lock every other node out of the account.
// When the store has a newer copy it is adopted first, and the refresh is
// skipped if that copy no longer needs one.
func (s refreshService) claimBackgroundRefresh(ctx context.Context, auth *Auth) refreshClaim {
	coordinator := s.manager.currentRefreshCoordinator()
	if coordinator == nil {
		return refreshClaim{proceed: true, auth: auth}
	}
	latest, acquired, err := coordinator.ClaimRefresh(ctx, auth.ID)
	if err != nil {
		if !errors.Is(err, ErrCredentialGone) {
			log.Warnf("auth refresh: skipping %s, refresh lease unavailable: %v", auth.ID, err)
		}
		return refreshClaim{}
	}
	if !acquired {
		log.Debugf("auth refresh: %s is being refreshed by another node", auth.ID)
		return refreshClaim{}
	}
	claim := refreshClaim{proceed: true, auth: auth, release: func() {
		coordinator.ReleaseRefresh(context.Background(), auth.ID)
	}}
	if latest == nil {
		return claim
	}
	if CredentialVersion(latest) > CredentialVersion(auth) {
		claim.auth = s.manager.AdoptPersistedCredential(ctx, latest)
	} else if refreshedAt, ok := authLastRefreshTimestamp(latest); ok && refreshedAt.After(auth.LastRefreshedAt) {
		claim.auth = auth.Clone()
		claim.auth.LastRefreshedAt = refreshedAt
		s.manager.noteRefreshedAt(auth.ID, refreshedAt)
	}
	probe := claim.auth.Clone()
	probe.NextRefreshAfter = time.Time{}
	if !s.shouldRefresh(probe, time.Now()) {
		log.Debugf("auth refresh: %s was already refreshed by another node", auth.ID)
		claim.done()
		return refreshClaim{}
	}
	return claim
}

// noteRefreshedAt records that id was refreshed at ts elsewhere, so the local
// interval check does not refresh it again right away.
func (m *Manager) noteRefreshedAt(id string, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.auths[id]; current != nil && ts.After(current.LastRefreshedAt) {
		current.LastRefreshedAt = ts
	}
}

// refreshBaseline snapshots a credential before a refresh mutates it, when a
// versioned store may need to re-apply the refresh onto a newer copy.
func (m *Manager) refreshBaseline(auth *Auth) MetadataSnapshot {
	if m.versionedStore() == nil || auth == nil {
		return nil
	}
	return SnapshotMetadata(auth.Metadata)
}

// persistRefreshResult saves a successful background refresh. With a
// versioned store a write that still fails after the retries keeps the new
// tokens on this node, so it can serve with them, and says so loudly.
func (m *Manager) persistRefreshResult(ctx context.Context, updated *Auth, before MetadataSnapshot) {
	if before == nil {
		_, _ = m.Update(ctx, updated)
		return
	}
	if _, err := m.UpdateRefreshed(ctx, updated, before); err != nil {
		if errors.Is(err, ErrCredentialGone) {
			return
		}
		log.Errorf("auth refresh: persisting refreshed credential %s failed, keeping it on this node only: %v", updated.ID, err)
		_, _ = m.Update(WithSkipPersist(ctx), updated)
	}
}

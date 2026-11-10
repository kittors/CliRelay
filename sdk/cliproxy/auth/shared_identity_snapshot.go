package auth

import "sort"

// SharedAccountIdentity is what a tenant must not learn of a platform account
// shared with it, and what it sees instead: a snapshot for masking, without the
// credential, its model states or any other map of the Auth, so a caller may
// keep it without the manager's lock. StandIns is the caller's own slice.
type SharedAccountIdentity struct {
	AuthID   string
	Index    string
	Name     string
	StandIns [][2]string
}

// ListSharedIdentitiesForTenant returns the identities of the platform
// accounts shared with tenantID as they are now: the grants and the accounts
// are read together, under the manager's lock. Unlike ListSharedWithTenant it
// clones no Auth, so it is cheap enough to call on every request log write; it
// walks the grants rather than every credential, so a server that shares
// nothing pays one pointer load.
func (m *Manager) ListSharedIdentitiesForTenant(tenantID string) []SharedAccountIdentity {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	grants := m.sharedGrantIndex()
	if grants == nil || len(grants.byAuth) == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var identities []SharedAccountIdentity
	for id := range grants.byAuth {
		a := m.auths[id]
		if !authSharedWithTenant(a, tenantID, grants) {
			continue
		}
		// currentIndex, not EnsureIndex: this is the manager's own copy, which
		// other goroutines read under the same read lock.
		identities = append(identities, SharedAccountIdentity{AuthID: a.ID, Index: a.currentIndex(), Name: a.SharedName(), StandIns: a.SharedStandIns()})
	}
	// map order is random; the masks are applied one after another, so keep
	// their order the same from one write to the next
	sort.Slice(identities, func(i, j int) bool { return identities[i].AuthID < identities[j].AuthID })
	return identities
}

package auth

// SharedAccountIdentity is an immutable display/masking snapshot. It omits
// credentials, model state and other mutable Auth maps; callers may retain it
// without holding the manager lock. StandIns owns its slice.
type SharedAccountIdentity struct {
	AuthID   string
	Index    string
	Name     string
	StandIns [][2]string
}

// ListSharedIdentitiesForTenant reads the live grants and account identities
// together for masking. In contrast to ListSharedWithTenant it does not clone
// full credential objects or model state on each request-log operation.
func (m *Manager) ListSharedIdentitiesForTenant(tenantID string) []SharedAccountIdentity {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	grants := m.sharedGrantIndex()
	if grants == nil || len(grants.byAuth) == 0 {
		return nil
	}
	identities := make([]SharedAccountIdentity, 0)
	// Most accounts are private. Walk the immutable grant index rather than all
	// credentials; empty/unshared installations avoid a per-log full-table scan.
	for id := range grants.byAuth {
		a := m.auths[id]
		if authSharedWithTenant(a, tenantID, grants) {
			identities = append(identities, SharedAccountIdentity{AuthID: a.ID, Index: a.currentIndex(), Name: a.SharedName(), StandIns: a.SharedStandIns()})
		}
	}
	return identities
}

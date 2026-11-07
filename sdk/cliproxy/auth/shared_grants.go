package auth

import "strings"

// Platform-shared credentials.
//
// The system tenant can share one of its credentials with other tenants. The
// credential stays a single Auth: one refresh token, one cooldown, one quota
// gate, one concurrency budget. Copies would refresh independently, and
// OpenAI/Anthropic rotate refresh tokens on every use, so each copy would
// revoke the other's.
//
// A shared credential always runs on its owner's executor: the system
// tenant's proxy pool, fingerprint and header settings, the same ones its
// token refresh uses. Running it on the requesting tenant's executor would send
// one account out through two egress addresses, which upstreams treat as
// account sharing (unsupported_country_region_territory and worse).
//
// Selection, channel-group membership and model gates still use the
// requesting tenant's routing config, so a shared credential takes part in the
// tenant's groups exactly like one of its own. Management mutations stay
// owner-only: ListForTenant and every lookup built on it keep returning owned
// credentials, so a grantee can never download, edit, delete or api-call one.

// sharedAuthEveryTenant grants a credential to every tenant. It mirrors
// resourcegrants.AllTenants, which this SDK package does not import.
const sharedAuthEveryTenant = "*"

type sharedAuthGrant struct {
	everyTenant bool
	tenants     map[string]struct{}
}

// sharedAuthGrantIndex is immutable once stored, so readers need no lock.
type sharedAuthGrantIndex struct {
	byAuth map[string]sharedAuthGrant
}

func (idx *sharedAuthGrantIndex) allows(authID, tenantID string) bool {
	if idx == nil || len(idx.byAuth) == 0 {
		return false
	}
	grant, ok := idx.byAuth[authID]
	if !ok {
		return false
	}
	if grant.everyTenant {
		return true
	}
	_, ok = grant.tenants[tenantID]
	return ok
}

// SetSharedAuthGrants replaces the set of system credentials shared with other
// tenants: credential ID -> grantee tenant IDs, "*" meaning every tenant.
// Grants naming a credential the manager does not hold are kept; they apply
// once that credential is registered under the same ID.
func (m *Manager) SetSharedAuthGrants(grants map[string][]string) {
	if m == nil {
		return
	}
	idx := &sharedAuthGrantIndex{byAuth: make(map[string]sharedAuthGrant, len(grants))}
	for authID, tenants := range grants {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		grant := sharedAuthGrant{tenants: make(map[string]struct{}, len(tenants))}
		for _, tenant := range tenants {
			tenant = strings.TrimSpace(tenant)
			switch {
			case tenant == "":
			case tenant == sharedAuthEveryTenant:
				grant.everyTenant = true
			default:
				grant.tenants[normalizedTenantID(tenant)] = struct{}{}
			}
		}
		if grant.everyTenant || len(grant.tenants) > 0 {
			idx.byAuth[authID] = grant
		}
	}
	m.sharedAuthGrants.Store(idx)
}

func (m *Manager) sharedGrantIndex() *sharedAuthGrantIndex {
	if m == nil {
		return nil
	}
	return m.sharedAuthGrants.Load()
}

// authUsableByTenant reports whether auth may serve requests of tenantID: one
// of the tenant's own credentials, or a system credential shared with it.
func authUsableByTenant(auth *Auth, tenantID string, grants *sharedAuthGrantIndex) bool {
	if auth == nil {
		return false
	}
	owner := normalizedTenantID(auth.TenantID)
	if owner == tenantID {
		return true
	}
	return owner == defaultTenantID && grants.allows(auth.ID, tenantID)
}

// authSharedWithTenant reports whether auth is a system credential shared with
// tenantID, as opposed to one tenantID owns.
func authSharedWithTenant(auth *Auth, tenantID string, grants *sharedAuthGrantIndex) bool {
	if auth == nil {
		return false
	}
	owner := normalizedTenantID(auth.TenantID)
	return owner != tenantID && owner == defaultTenantID && grants.allows(auth.ID, tenantID)
}

// IsSharedWithTenant reports whether auth is a system credential that the
// platform shares with tenantID. The system tenant owns its credentials, so
// nothing is shared with it.
func (m *Manager) IsSharedWithTenant(auth *Auth, tenantID string) bool {
	if m == nil {
		return false
	}
	return authSharedWithTenant(auth, normalizedTenantID(tenantID), m.sharedGrantIndex())
}

// ListUsableForTenant returns the credentials that serve tenantID's requests:
// its own and the system credentials shared with it. Use it for what a tenant
// can route to and list models from; use ListForTenant for what it may manage.
func (m *Manager) ListUsableForTenant(tenantID string) []*Auth {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	grants := m.sharedGrantIndex()
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0)
	for _, auth := range m.auths {
		if authUsableByTenant(auth, tenantID, grants) {
			list = append(list, auth.Clone())
		}
	}
	return list
}

// ListSharedWithTenant returns only the system credentials shared with
// tenantID, for read-only display to that tenant.
func (m *Manager) ListSharedWithTenant(tenantID string) []*Auth {
	if m == nil {
		return nil
	}
	tenantID = normalizedTenantID(tenantID)
	grants := m.sharedGrantIndex()
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0)
	for _, auth := range m.auths {
		if authSharedWithTenant(auth, tenantID, grants) {
			list = append(list, auth.Clone())
		}
	}
	return list
}

// executorForAuthLocked returns the executor a selected credential runs on:
// the one bound to its owner's runtime config. Callers hold m.mu.
func (m *Manager) executorForAuthLocked(auth *Auth, provider string) ProviderExecutor {
	if auth == nil {
		return nil
	}
	return m.executorForTenantLocked(auth.TenantID, provider)
}

// sharesProviderWithTenantLocked reports whether some system credential of
// provider, with an executor to run on, is shared with tenantID. Callers hold
// m.mu.
func (m *Manager) sharesProviderWithTenantLocked(tenantID, provider string, grants *sharedAuthGrantIndex) bool {
	if m == nil || grants == nil || len(grants.byAuth) == 0 {
		return false
	}
	for _, auth := range m.auths {
		if auth == nil || auth.Provider != provider || !authSharedWithTenant(auth, tenantID, grants) {
			continue
		}
		if m.executorForAuthLocked(auth, provider) != nil {
			return true
		}
	}
	return false
}

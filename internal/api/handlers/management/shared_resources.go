package management

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// Platform-shared resources: proxies and AI account credentials the system
// tenant owns and the platform administrator grants to other tenants. The
// grant model is documented in sqlstore/resourcegrants; the runtime side in
// sdk/cliproxy/auth/shared_grants.go and internal/app/service/shared_resources.go.

// resourceSharing describes who a shared resource is granted to.
type resourceSharing struct {
	All       bool     `json:"all"`
	TenantIDs []string `json:"tenant_ids"`
}

// sharingCapability tells the panel whether this backend shares resources and
// whether the caller may change what is shared. The panel updates itself
// independently of the backend, so it must not assume either.
type sharingCapability struct {
	Supported  bool `json:"supported"`
	Manageable bool `json:"manageable"`
}

// tenantRef names a tenant in sharing responses.
type tenantRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type sharingRequest struct {
	All       bool     `json:"all"`
	TenantIDs []string `json:"tenant_ids"`
}

func sharingFromGrantees(grantees []string) *resourceSharing {
	grantees = resourcegrants.NormalizeTenants(grantees)
	if len(grantees) == 0 {
		return nil
	}
	if len(grantees) == 1 && grantees[0] == resourcegrants.AllTenants {
		return &resourceSharing{All: true, TenantIDs: []string{}}
	}
	return &resourceSharing{TenantIDs: grantees}
}

// sharingContext is the request context, or the background one for callers
// that assemble a gin context without a request (unit tests).
func sharingContext(c *gin.Context) context.Context {
	if c == nil || c.Request == nil {
		return context.Background()
	}
	return c.Request.Context()
}

func callerIsPlatformAdmin(c *gin.Context) bool {
	principal, ok := principalFromContext(c)
	return ok && principal.PlatformAdmin
}

// canManageSharing reports whether the caller is a platform administrator
// acting in the system tenant, the only owner of shared resources.
func canManageSharing(c *gin.Context) bool {
	return effectiveTenantID(c) == identity.SystemTenantID && callerIsPlatformAdmin(c)
}

func sharingCapabilityFor(c *gin.Context) sharingCapability {
	return sharingCapability{Supported: usage.ResourceGrantsAvailable(), Manageable: canManageSharing(c)}
}

// requireSharingManager answers the request itself and returns false unless
// the caller may change what the platform shares.
func requireSharingManager(c *gin.Context) bool {
	if !usage.ResourceGrantsAvailable() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "resource sharing requires the database"})
		return false
	}
	if effectiveTenantID(c) != identity.SystemTenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "only resources of the system tenant can be shared; switch to it first"})
		return false
	}
	if !callerIsPlatformAdmin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only platform administrators can change what is shared"})
		return false
	}
	return true
}

// resolveGrantees validates a sharing request against the tenants that exist
// and returns the grantee list to store: "*" for every tenant, the explicit
// tenants otherwise, nothing to stop sharing.
func resolveGrantees(ctx context.Context, req sharingRequest) ([]string, error) {
	if req.All {
		return []string{resourcegrants.AllTenants}, nil
	}
	requested := resourcegrants.NormalizeTenants(req.TenantIDs)
	if len(requested) == 0 {
		return nil, nil
	}
	known, err := tenantDirectory(ctx)
	if err != nil {
		return nil, err
	}
	for _, tenantID := range requested {
		if tenantID == resourcegrants.AllTenants {
			return nil, fmt.Errorf("use all=true to share with every tenant")
		}
		if tenantID == identity.SystemTenantID {
			return nil, fmt.Errorf("the system tenant owns shared resources and cannot be granted them")
		}
		if _, ok := known[tenantID]; !ok {
			return nil, fmt.Errorf("unknown tenant: %s", tenantID)
		}
	}
	return requested, nil
}

// tenantDirectory maps tenant IDs to tenants.
func tenantDirectory(ctx context.Context) (map[string]identity.Tenant, error) {
	service := identity.Default()
	if service == nil {
		return nil, fmt.Errorf("tenant directory unavailable")
	}
	tenants, err := service.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]identity.Tenant, len(tenants))
	for _, tenant := range tenants {
		out[tenant.ID] = tenant
	}
	return out, nil
}

// tenantRefs names tenantIDs for display, sorted by name. IDs the directory
// does not know are listed by ID rather than dropped: a row the operator
// cannot see would be a grant nobody can revoke.
func tenantRefs(ctx context.Context, tenantIDs []string) []tenantRef {
	if len(tenantIDs) == 0 {
		return []tenantRef{}
	}
	directory, _ := tenantDirectory(ctx)
	refs := make([]tenantRef, 0, len(tenantIDs))
	for _, id := range tenantIDs {
		name := id
		if tenant, ok := directory[id]; ok && strings.TrimSpace(tenant.Name) != "" {
			name = tenant.Name
		}
		refs = append(refs, tenantRef{ID: id, Name: name})
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

// refreshSharedProxyPools re-applies tenant proxy pools on this node after
// the platform pool or the proxy grants changed. Other nodes do the same on
// the configsync event the write announced.
func (h *Handler) refreshSharedProxyPools() {
	if h == nil || h.authManager == nil {
		return
	}
	h.mu.Lock()
	base := h.cfg
	h.mu.Unlock()
	serviceapp.RefreshSharedProxyPools(base, h.authManager)
}

// reloadSharedAuthGrants applies credential grants on this node right away.
func (h *Handler) reloadSharedAuthGrants() {
	if h == nil || h.authManager == nil {
		return
	}
	serviceapp.LoadSharedAuthGrants(h.authManager)
}

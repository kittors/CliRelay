package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	proxypoolsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/resourcegrants"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// sharedProxyAPIEntry is a platform proxy as a grantee sees it. It never
// carries the URL: the address and credentials stay with the platform, and
// the masked form (scheme and host) is enough to recognise the exit.
type sharedProxyAPIEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Description string `json:"description,omitempty"`
	MaskedURL   string `json:"masked_url"`
	// Overridden marks a shared entry the tenant's own entry of the same ID
	// replaces; the tenant's accounts use its own entry.
	Overridden bool `json:"overridden"`
}

// decorateProxyPoolResponse adds the sharing view to a GET /proxy-pool body:
// the grants and overrides of each platform entry for the system tenant, the
// entries shared with it for any other tenant.
func decorateProxyPoolResponse(c *gin.Context, tenantID string, items []proxyPoolAPIEntry, body gin.H) {
	body["sharing"] = sharingCapabilityFor(c)
	if !usage.ResourceGrantsAvailable() {
		return
	}
	ctx := sharingContext(c)
	if tenantID != identity.SystemTenantID {
		own := make(map[string]struct{}, len(items))
		for _, item := range items {
			own[config.NormalizeProxyID(item.ID)] = struct{}{}
		}
		shared := proxypoolsettings.SharedForTenant(tenantID)
		sharedItems := make([]sharedProxyAPIEntry, 0, len(shared))
		for _, entry := range shared {
			_, overridden := own[config.NormalizeProxyID(entry.ID)]
			sharedItems = append(sharedItems, sharedProxyAPIEntry{
				ID:          entry.ID,
				Name:        entry.Name,
				Enabled:     entry.Enabled,
				Description: entry.Description,
				MaskedURL:   maskProxyPoolURL(entry.URL),
				Overridden:  overridden,
			})
		}
		body["shared_items"] = sharedItems
		return
	}
	grants, err := usage.ListResourceGrants(ctx, resourcegrants.KindProxy)
	if err != nil || len(grants) == 0 {
		return
	}
	sharedIDs := make([]string, 0, len(grants))
	for i := range items {
		if sharing := sharingFromGrantees(grants[items[i].ID]); sharing != nil {
			items[i].Sharing = sharing
			sharedIDs = append(sharedIDs, items[i].ID)
		}
	}
	holders, err := proxypoolsettings.TenantsOverriding(ctx, sharedIDs)
	if err != nil {
		return
	}
	for i := range items {
		var overriding []string
		for _, tenantID := range holders[items[i].ID] {
			// Only a tenant the entry is shared with is overriding it; anybody
			// else simply has an unrelated entry under the same ID.
			if resourcegrants.Allows(grants[items[i].ID], tenantID) {
				overriding = append(overriding, tenantID)
			}
		}
		if len(overriding) > 0 {
			items[i].OverriddenBy = tenantRefs(ctx, overriding)
		}
	}
}

// PutProxyPoolSharing sets which tenants may use one of the system tenant's
// proxies: {"all": true} for every tenant, {"tenant_ids": [...]} for some,
// neither to stop sharing it.
func (h *Handler) PutProxyPoolSharing(c *gin.Context) {
	if !requireSharingManager(c) {
		return
	}
	id := config.NormalizeProxyID(c.Param("id"))
	if id == "" || proxypoolsettings.GetForTenant(identity.SystemTenantID, id) == nil {
		writeNotFoundError(c, "系统代理条目", id)
		return
	}
	var req sharingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeInvalidBodyError(c, err)
		return
	}
	grantees, err := resolveGrantees(sharingContext(c), req)
	if err != nil {
		writeInvalidFieldError(c, "tenant_ids", err.Error())
		return
	}
	if _, err := usage.ReplaceResourceGrants(sharingContext(c), resourcegrants.KindProxy, []string{id}, grantees); err != nil {
		writeInternalError(c, "保存代理共享配置", err)
		return
	}
	h.refreshSharedProxyPools()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "sharing": sharingFromGrantees(grantees)})
}

// PostProxyPoolAdopt switches the tenants that override a shared proxy with
// their own entry of the same ID over to the shared one, by removing their
// entry. Accounts reference proxies by ID, so they keep their setting and
// resolve the shared entry from then on. Tenants the proxy is not shared with
// are left alone: removing their entry would leave their accounts without it.
func (h *Handler) PostProxyPoolAdopt(c *gin.Context) {
	if !requireSharingManager(c) {
		return
	}
	ctx := sharingContext(c)
	id := config.NormalizeProxyID(c.Param("id"))
	shared := proxypoolsettings.GetForTenant(identity.SystemTenantID, id)
	if id == "" || shared == nil {
		writeNotFoundError(c, "系统代理条目", id)
		return
	}
	if !shared.Enabled {
		writeError(c, http.StatusConflict, "shared_proxy_disabled", "该共享代理当前处于禁用状态，请先启用后再让租户切换采用")
		return
	}
	grants, err := usage.ListResourceGrants(ctx, resourcegrants.KindProxy)
	if err != nil {
		writeInternalError(c, "读取代理共享授权列表", err)
		return
	}
	grantees := grants[id]
	if len(grantees) == 0 {
		writeError(c, http.StatusConflict, "proxy_not_shared", "该代理尚未共享给任何租户，无法执行统一切换")
		return
	}
	holders, err := proxypoolsettings.TenantsOverriding(ctx, []string{id})
	if err != nil {
		writeInternalError(c, "读取覆盖该代理的租户列表", err)
		return
	}
	adopted := make([]string, 0, len(holders[id]))
	for _, tenantID := range holders[id] {
		if !resourcegrants.Allows(grantees, tenantID) {
			continue
		}
		if err := removeTenantProxyEntry(ctx, tenantID, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{
					"code":    "adopt_proxy_failed",
					"message": fmt.Sprintf("切换租户 %q 的共享代理配置失败，部分租户已完成切换，请稍后重试或查看服务端日志", tenantID),
					"details": gin.H{"tenant_id": tenantID},
				},
				"tenants": tenantRefs(ctx, adopted),
			})
			return
		}
		adopted = append(adopted, tenantID)
		h.applyTenantProxyPoolConfig(tenantID)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "tenants": tenantRefs(ctx, adopted)})
}

// removeTenantProxyEntry drops one entry from a tenant's pool. The pool is a
// whole-replace collection, so the write is checked against the version the
// list was read at and retried if the tenant changed it meanwhile, rather
// than overwriting that change.
func removeTenantProxyEntry(ctx context.Context, tenantID, id string) error {
	const attempts = 3
	var lastErr error
	for range attempts {
		version := proxypoolsettings.CollectionVersionForTenant(tenantID)
		current := proxypoolsettings.ListForTenant(tenantID)
		kept := make([]config.ProxyPoolEntry, 0, len(current))
		for _, entry := range current {
			if config.NormalizeProxyID(entry.ID) != id {
				kept = append(kept, entry)
			}
		}
		if len(kept) == len(current) {
			return nil
		}
		_, err := proxypoolsettings.ReplaceForTenantExpect(ctx, tenantID, kept, version)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, configsync.ErrVersionConflict) {
			return err
		}
	}
	return lastErr
}

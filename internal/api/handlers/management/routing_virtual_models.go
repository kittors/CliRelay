package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Virtual models live in the tenant's routing config, next to the channel
// groups their members name (internal/config/routing_virtual_models.go). Two
// writers edit them: PUT /routing-config, which replaces the whole config, and
// PUT /routing-config/virtual-models, which replaces them alone so that the
// model editor never writes back channel-group fields it does not model.

// GetRoutingVirtualModels answers GET /routing-config/virtual-models with the
// tenant's virtual models and the routing config's version.
func (h *Handler) GetRoutingVirtualModels(c *gin.Context) {
	routing, version := h.storedRoutingConfig(effectiveTenantID(c))
	models := routing.VirtualModels
	if models == nil {
		models = []config.VirtualModel{}
	}
	setVersionHeader(c, version)
	c.JSON(http.StatusOK, gin.H{"virtual-models": models, "version": version})
}

// PutRoutingVirtualModels answers PUT /routing-config/virtual-models: it
// replaces the tenant's virtual models and keeps the rest of its routing config.
// The write is checked against the version the client read, or, when it sent
// none, the one this request read: either way a concurrent change of the
// routing config is refused rather than overwritten.
func (h *Handler) PutRoutingVirtualModels(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	var body struct {
		VirtualModels *[]config.VirtualModel `json:"virtual-models"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.VirtualModels == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: virtual-models is required"})
		return
	}
	sent := *body.VirtualModels

	routing, version := h.storedRoutingConfig(tenantID)
	expected := requestExpectedVersion(c)
	if expected == configsync.AnyVersion {
		expected = version
	}
	candidate := &config.Config{
		SDKConfig: config.SDKConfig{APIKeyEntries: storedAPIKeyEntries(tenantID)},
		Routing:   routing,
	}
	candidate.Routing.VirtualModels = sent
	var auths []*coreauth.Auth
	if h != nil && h.authManager != nil {
		auths = h.authManager.ListUsableForTenant(tenantID)
	}
	if err := config.ValidateVirtualModels(sent, knownRoutingGroups(candidate, auths)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	candidate.SanitizeRouting()
	stored, err := usage.CompareAndSwapRoutingConfigForTenant(c.Request.Context(), tenantID, candidate.Routing, expected)
	if err != nil {
		writeConfigSaveError(c, "failed to save virtual models", err)
		return
	}
	h.applyRoutingConfig(c, tenantID, candidate.Routing, stored)
}

// bindRoutingConfigBody decodes the routing config of a PUT /routing-config,
// and reports whether it named virtual-models at all: a client that predates
// them leaves the key out, which is not a request to delete them.
func bindRoutingConfigBody(c *gin.Context) (config.RoutingConfig, bool, error) {
	var body config.RoutingConfig
	data, err := c.GetRawData()
	if err != nil {
		return body, false, err
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return body, false, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return body, false, err
	}
	_, sent := keys["virtual-models"]
	return body, sent, nil
}

// storedRoutingConfig returns the tenant's routing config and the version it
// is stored at, zero when it has none stored.
func (h *Handler) storedRoutingConfig(tenantID string) (config.RoutingConfig, int64) {
	stored, version := usage.GetRoutingConfigWithVersionForTenant(tenantID)
	if stored != nil {
		return *stored, version
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.cfg
	}
	return currentRoutingConfigForTenant(cfg, tenantID), version
}

// validateRoutingVirtualModels checks the virtual models of a routing config
// about to be saved. The ones the client sent are checked as it sent them,
// before SanitizeRouting quietly dropped what it could not keep. The ones
// carried over from the stored config only need their channel groups to still
// exist, which a save that removes a group would otherwise break.
func validateRoutingVirtualModels(candidate *config.Config, auths []*coreauth.Auth, sent []config.VirtualModel, wasSent bool) error {
	known := knownRoutingGroups(candidate, auths)
	if wasSent {
		return config.ValidateVirtualModels(sent, known)
	}
	used := candidate.Routing.VirtualModelGroups()
	groups := make([]string, 0, len(used))
	for group := range used {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	for _, group := range groups {
		if _, ok := known[group]; !ok {
			return fmt.Errorf("channel group %q is used by virtual model %q; change the virtual model before removing the group", group, used[group])
		}
	}
	return nil
}

// knownRoutingGroups names the channel groups a routing config may refer to,
// as path routes may (validateRoutingAndAPIKeyRestrictions).
func knownRoutingGroups(cfg *config.Config, auths []*coreauth.Auth) map[string]struct{} {
	groups := buildChannelGroupItems(cfg, auths)
	known := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		known[group.Name] = struct{}{}
	}
	return known
}

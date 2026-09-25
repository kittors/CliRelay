package management

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// putRoutingStrategy stores the strategy in the routing config row.
//
// It used to change only the in-memory config and config.yaml. Routing lives
// in routing_config, and every reload overlays that row onto the config, so
// the new strategy was reverted by the next reload, and in a cluster it never
// reached the other nodes at all.
func (h *Handler) putRoutingStrategy(c *gin.Context, strategy string) {
	if !usage.ConfigStoreAvailable() {
		h.mu.Lock()
		h.cfg.Routing.Strategy = strategy
		h.mu.Unlock()
		h.persist(c)
		return
	}
	stored, version := usage.GetRoutingConfigWithVersionForTenant(identity.SystemTenantID)
	routing := currentRoutingConfig(h.cfg)
	if stored != nil {
		routing = *stored
	}
	routing.Strategy = strategy
	// Only the strategy changes, applied to the stored config it was read
	// from; the client's version, when sent, is checked instead.
	expected := version
	if clientVersion := requestVersion(c); clientVersion != nil {
		expected = *clientVersion
	}
	newVersion, err := usage.CompareAndSwapRoutingConfigForTenant(c.Request.Context(), identity.SystemTenantID, routing, expected)
	if err != nil {
		writeConfigSaveError(c, "failed to save routing strategy", err)
		return
	}
	h.mu.Lock()
	h.cfg.Routing = routing
	cfg := h.cfg
	mutated := h.onConfigMutated
	h.mu.Unlock()
	if h.authManager != nil {
		h.authManager.SetConfig(cfg)
	}
	setVersionHeader(c, newVersion)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if mutated != nil {
		mutated(cfg)
	}
}

// persistDerivedRouting applies a derived change to the stored system routing
// config, retrying on concurrent writes, and adopts the result. The live
// routing, already changed by the caller, is written when no row exists yet.
func (h *Handler) persistDerivedRouting(mutate func(*config.RoutingConfig) bool) error {
	if !usage.ConfigStoreAvailable() {
		return nil
	}
	h.mu.Lock()
	fallback := h.cfg.Routing
	h.mu.Unlock()
	routing, _, err := usage.UpdateRoutingConfigForTenant(context.Background(), identity.SystemTenantID, fallback, mutate)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.cfg.Routing = routing
	h.mu.Unlock()
	return nil
}

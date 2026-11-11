package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	proxypoolsettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/proxypool"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/egressbreaker"
)

// What the egress breaker (package egressbreaker) knows about the proxies of
// the pool, for the console. Since the breaker took over, a proxy that fails
// no longer shows on the accounts behind it, and the requests it turned away
// never reach the request log, so this is where an operator sees it.
//
// The state lives in the memory of the process that sent the requests, so
// this is what the process answering the request has seen: the whole picture
// on a single node.

// proxyEgressStatus is one pool entry the breaker refuses requests through.
type proxyEgressStatus struct {
	ID string `json:"id"`
	// Shared marks a platform entry shared with the tenant (the console lists
	// those apart); false for the tenant's own entries.
	Shared bool `json:"shared"`
	// Fault is "auth_rejected" or "unreachable".
	Fault string    `json:"fault"`
	Since time.Time `json:"since"`
	// NextProbeAt is when the next request is let through to try the proxy. It
	// is in the past when no request has come since; the next one will.
	NextProbeAt time.Time `json:"next_probe_at"`
	// Probing says that request is out now.
	Probing bool `json:"probing"`
	// Refused counts the attempts turned away, one per account a request tried.
	Refused      int64 `json:"refused"`
	FailedProbes int   `json:"failed_probes"`
}

type proxyEgressStatusResponse struct {
	// Enabled is false when the breaker is switched off on this node
	// (CLIRELAY_EGRESS_BREAKER=off): nothing is refused, nothing is known.
	Enabled bool `json:"enabled"`
	// Items holds the entries whose breaker is open; an entry not listed lets
	// requests through.
	Items []proxyEgressStatus `json:"items"`
}

// GetProxyPoolEgressStatus lists the tenant's proxy entries, its own and the
// ones shared with it, that the egress breaker refuses requests through. It is
// cheap and changes nothing, so the console polls it; it never returns an
// address or a credential.
func (h *Handler) GetProxyPoolEgressStatus(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	breaker := h.egressBreaker()
	body := proxyEgressStatusResponse{Enabled: breaker.Enabled(), Items: []proxyEgressStatus{}}
	own, shared := h.tenantProxyPools(tenantID)
	add := func(entries []config.ProxyPoolEntry, isShared bool) {
		for _, entry := range entries {
			// A switched-off entry carries no traffic: accounts that name it
			// fall back to another proxy.
			url := strings.TrimSpace(entry.URL)
			if !entry.Enabled || url == "" {
				continue
			}
			observation := breaker.Observe(url)
			if !observation.Open {
				continue
			}
			body.Items = append(body.Items, proxyEgressStatus{
				ID: config.NormalizeProxyID(entry.ID), Shared: isShared,
				Fault: observation.Fault.Code(), Since: observation.Since.UTC(),
				NextProbeAt: observation.NextProbe.UTC(), Probing: observation.Probing,
				Refused: observation.Refused, FailedProbes: observation.FailedProbes,
			})
		}
	}
	add(own, false)
	add(shared, true)
	c.JSON(http.StatusOK, body)
}

// egressBreaker is the breaker the upstream requests of this process pass.
func (h *Handler) egressBreaker() *egressbreaker.Breaker {
	if h != nil && h.egress != nil {
		return h.egress
	}
	return egressbreaker.Default()
}

// tenantProxyPools returns the tenant's own proxy entries and the platform
// entries shared with it, overridden ones included (the caller decides what
// an override means for it). The system tenant has no shared entries: the
// shared ones are its own.
func (h *Handler) tenantProxyPools(tenantID string) (own, shared []config.ProxyPoolEntry) {
	own = h.ownProxyPool(tenantID)
	if tenantID != identity.SystemTenantID {
		shared = proxypoolsettings.SharedForTenant(tenantID)
	}
	return own, shared
}

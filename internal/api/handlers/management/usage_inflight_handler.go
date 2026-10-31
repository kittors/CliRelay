package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// GetInFlightRequests lists the requests of the effective tenant this process
// is serving right now, oldest first: GET /usage/monitor/in-flight.
//
// It shares monitor.read with the rest of the monitor center. A row carries the
// columns the monitor's recent failures already show to that permission (time,
// model, account, key name) plus the stage the request is in, and no request
// content, so it reveals nothing a monitor reader cannot see once the request
// has finished. Requests without a tenant belong to the system tenant, as
// their request log rows do. In a cluster each node lists its own requests.
func (h *UsageLogsHandler) GetInFlightRequests(c *gin.Context) {
	tenantID := coreauth.NormalizedTenantID(effectiveTenantID(c))
	now := time.Now()
	requests, total := diagnostics.InFlight(func(requestTenant string) bool {
		return coreauth.NormalizedTenantID(requestTenant) == tenantID
	}, now)
	c.JSON(http.StatusOK, gin.H{
		"requests":     requests,
		"total":        total,
		"generated_at": now.Format(time.RFC3339Nano),
	})
}

// monitorOverviewResponse is GET /usage/monitor/overview with what the
// backend's monitor center offers beyond it. The panel follows the latest
// codeProxy release, not this binary, so it shows a card only when the
// backend it talks to says so.
type monitorOverviewResponse struct {
	usage.MonitorOverview
	Capabilities monitorCapabilities `json:"capabilities"`
}

type monitorCapabilities struct {
	// InFlightRequests: GET /usage/monitor/in-flight answers.
	InFlightRequests bool `json:"in-flight-requests"`
}

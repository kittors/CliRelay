package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	managementusagelogs "github.com/router-for-me/CLIProxyAPI/v6/internal/management/usagelogs"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// GetUsageMonitorOverview serves the monitor center:
// GET /usage/monitor/overview?range=&consumer=&model=&channel=
//
// A request without parameters is valid and returns the default 24h window,
// which is also what the PostgreSQL route smoke test exercises.
func (h *UsageLogsHandler) GetUsageMonitorOverview(c *gin.Context) {
	query, ok := monitorQueryFromRequest(c)
	if !ok {
		return
	}
	payload, err := h.service(c).MonitorOverview(query)
	if err != nil {
		writeInternalError(c, "查询监控总览数据", err)
		return
	}
	c.JSON(http.StatusOK, monitorOverviewResponse{
		MonitorOverview: payload,
		Capabilities:    monitorCapabilities{InFlightRequests: true},
	})
}

// GetUsageMonitorRealtime serves the live strip: the last hour per minute.
func (h *UsageLogsHandler) GetUsageMonitorRealtime(c *gin.Context) {
	query, ok := monitorQueryFromRequest(c)
	if !ok {
		return
	}
	payload, err := h.service(c).MonitorRealtime(query)
	if err != nil {
		writeInternalError(c, "查询实时监控趋势数据", err)
		return
	}
	c.JSON(http.StatusOK, payload)
}

func monitorQueryFromRequest(c *gin.Context) (managementusagelogs.MonitorQuery, bool) {
	rangeKey, ok := usage.NormalizeMonitorRange(c.Query("range"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "invalid_range",
			"error": gin.H{
				"code":    "invalid_range",
				"message": "不支持的时间范围参数 range，仅支持 1h、6h、24h、today、7d、14d 或 30d",
			},
		})
		return managementusagelogs.MonitorQuery{}, false
	}
	return managementusagelogs.MonitorQuery{
		Range:     rangeKey,
		Consumers: repeatedQueryValues(c, "consumer"),
		Models:    repeatedQueryValues(c, "model"),
		Channels:  repeatedQueryValues(c, "channel"),
	}, true
}

// repeatedQueryValues reads ?key=a&key=b. Unlike queryStringList it does not
// split on commas: model and channel names are free text, and a channel named
// "Team A, Codex" must stay one value.
func repeatedQueryValues(c *gin.Context, key string) []string {
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, raw := range c.QueryArray(key) {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

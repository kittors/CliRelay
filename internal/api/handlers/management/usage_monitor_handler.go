package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	managementusagelogs "github.com/router-for-me/CLIProxyAPI/v6/internal/management/usagelogs"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
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
		log.Warnf("management usage monitor: overview failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

// GetUsageMonitorRealtime serves the live strip: the last hour per minute.
func (h *UsageLogsHandler) GetUsageMonitorRealtime(c *gin.Context) {
	query, ok := monitorQueryFromRequest(c)
	if !ok {
		return
	}
	payload, err := h.service(c).MonitorRealtime(query)
	if err != nil {
		log.Warnf("management usage monitor: realtime failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, payload)
}

func monitorQueryFromRequest(c *gin.Context) (managementusagelogs.MonitorQuery, bool) {
	rangeKey, ok := usage.NormalizeMonitorRange(c.Query("range"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid range: use 1h, 6h, 24h, today, 7d, 14d or 30d",
			"code":  "invalid_range",
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

package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/proxy"
)

type ProxyManagerHandler struct {
	pm *proxy.ProxyManager
}

func NewProxyManagerHandler(pm *proxy.ProxyManager) *ProxyManagerHandler {
	return &ProxyManagerHandler{pm: pm}
}

func (h *ProxyManagerHandler) GetProxyManagerStatus(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	status := gin.H{
		"enabled": h.pm.IsEnabled(),
	}

	if health := h.pm.GetHealthStatuses(); health != nil {
		status["health"] = health
	}

	if dist := h.pm.GetDistribution(); dist != nil {
		status["distribution"] = dist
	}

	c.JSON(http.StatusOK, status)
}

func (h *ProxyManagerHandler) GetProxyHealth(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":  true,
		"statuses": h.pm.GetHealthStatuses(),
	})
}

func (h *ProxyManagerHandler) GetProxyDistribution(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"enabled":      true,
		"distribution": h.pm.GetDistribution(),
	})
}

func (h *ProxyManagerHandler) GetBanEvents(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	proxyID := c.Query("proxy_id")
	limit := 50
	events := h.pm.GetBanEvents(proxyID, limit)

	c.JSON(http.StatusOK, gin.H{
		"enabled": true,
		"events":  events,
	})
}

func (h *ProxyManagerHandler) PostForceHealthCheck(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	disabled := h.pm.ForceHealthCheck(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{
		"disabled_proxies": disabled,
	})
}

func (h *ProxyManagerHandler) PostTriggerAssignment(c *gin.Context) {
	if h.pm == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}

	count := h.pm.TriggerAssignment()
	c.JSON(http.StatusOK, gin.H{
		"assigned_count": count,
	})
}

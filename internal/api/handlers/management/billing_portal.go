package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) portalBillingEnabled(c *gin.Context, user enduser.User) bool {
	svc := h.billingService()
	if svc == nil {
		return false
	}
	return svc.HasPortalBilling(c.Request.Context(), user.TenantID, user.ID)
}

func (h *Handler) GetPortalBillingOverview(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	overview, err := h.billingService().GetPortalOverview(c.Request.Context(), user.TenantID, user.ID)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, overview)
}

func (h *Handler) PostPortalBillingOrder(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	var input billing.CreateOrderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	order, err := h.billingService().CreatePortalOrder(
		c.Request.Context(),
		user.TenantID,
		user.ID,
		user.Username,
		billingBaseURLFromRequest(c),
		input,
	)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusCreated, order)
}

func (h *Handler) GetPortalBillingOrders(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	result, err := h.billingService().ListOrders(c.Request.Context(), user.TenantID, billing.OrderFilter{
		EndUserID: user.ID,
		Status:    c.Query("status"),
		Page:      page,
		Size:      size,
	})
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetPortalBillingOrder(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	sync := c.Query("sync") == "1" || strings.EqualFold(c.Query("sync"), "true")
	order, err := h.billingService().GetOrderForEndUser(c.Request.Context(), user.TenantID, user.ID, c.Param("order_no"), sync)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// ── Public direct-pay & XunhuPay callback endpoints ────────────────────────

func (h *Handler) GetPublicBillingPay(c *gin.Context) {
	info, err := h.billingService().GetPublicPayInfo(c.Request.Context(), c.Param("code"))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, info)
}

func (h *Handler) PostPublicBillingOrder(c *gin.Context) {
	var input billing.CreateOrderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	order, err := h.billingService().CreatePublicOrder(c.Request.Context(), c.Param("code"), billingBaseURLFromRequest(c), input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusCreated, order)
}

func (h *Handler) GetPublicBillingOrder(c *gin.Context) {
	sync := c.Query("sync") == "1" || strings.EqualFold(c.Query("sync"), "true")
	order, err := h.billingService().GetOrderForPublicCode(c.Request.Context(), c.Param("code"), c.Param("order_no"), sync)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, order)
}

// PostPublicXunhuPayCallback handles XunhuPay's server-to-server async payment notification.
// XunhuPay expects HTTP 200 with literal plain-text body "success" once handled.
func (h *Handler) PostPublicXunhuPayCallback(c *gin.Context) {
	if err := c.Request.ParseForm(); err != nil {
		c.String(http.StatusBadRequest, "fail")
		return
	}
	params := make(map[string]string, len(c.Request.PostForm))
	for k, vals := range c.Request.PostForm {
		if len(vals) > 0 {
			params[k] = vals[0]
		}
	}
	if len(params) == 0 {
		for k, vals := range c.Request.Form {
			if len(vals) > 0 {
				params[k] = vals[0]
			}
		}
	}
	tenantID := strings.TrimSpace(c.Param("tenant_id"))
	channel := strings.TrimSpace(c.Param("channel"))
	if err := h.billingService().HandleXunhuCallback(c.Request.Context(), tenantID, channel, params); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"tenant_id": tenantID,
			"channel":   channel,
			"order_no":  params["trade_order_id"],
		}).Warn("billing: xunhupay callback rejected")
		c.String(http.StatusBadRequest, "fail")
		return
	}
	c.String(http.StatusOK, "success")
}

// ── Key-authenticated billing endpoints for /#/apikey-usage ────────────────
// Plaintext api_key is always passed in the POST JSON body (never URL query),
// matching the security contract of /v0/management/public/usage/*.

func (h *Handler) resolvePublicBillingAPIKey(c *gin.Context, rawKey string) (*usage.APIKeyRow, bool) {
	apiKey := strings.TrimSpace(rawKey)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "api_key is required"}})
		return nil, false
	}
	if config.IsPlaceholderAPIKey(apiKey) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_api_key", "message": "Invalid API key"}})
		return nil, false
	}
	row := usage.GetAPIKey(apiKey)
	if row == nil || row.Disabled || strings.TrimSpace(row.ID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_api_key", "message": "Invalid API key"}})
		return nil, false
	}
	return row, true
}

func (h *Handler) PostPublicKeyBillingOverview(c *gin.Context) {
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	row, ok := h.resolvePublicBillingAPIKey(c, body.APIKey)
	if !ok {
		return
	}
	overview, err := h.billingService().GetAPIKeyOverview(c.Request.Context(), strings.TrimSpace(row.TenantID), strings.TrimSpace(row.ID))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, overview)
}

func (h *Handler) PostPublicKeyBillingOrder(c *gin.Context) {
	var body struct {
		APIKey            string `json:"api_key"`
		PlanID            string `json:"plan_id"`
		PayChannel        string `json:"pay_channel"`
		CustomAmountCents int    `json:"custom_amount_cents,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	row, ok := h.resolvePublicBillingAPIKey(c, body.APIKey)
	if !ok {
		return
	}
	order, err := h.billingService().CreateAPIKeyOrder(
		c.Request.Context(),
		strings.TrimSpace(row.TenantID),
		strings.TrimSpace(row.ID),
		billingBaseURLFromRequest(c),
		billing.CreateOrderInput{
			PlanID:            body.PlanID,
			PayChannel:        body.PayChannel,
			CustomAmountCents: body.CustomAmountCents,
		},
	)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusCreated, order)
}

func (h *Handler) PostPublicKeyBillingOrders(c *gin.Context) {
	var body struct {
		APIKey string `json:"api_key"`
		Page   int    `json:"page,omitempty"`
		Size   int    `json:"size,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	row, ok := h.resolvePublicBillingAPIKey(c, body.APIKey)
	if !ok {
		return
	}
	result, err := h.billingService().ListAPIKeyOrders(c.Request.Context(), strings.TrimSpace(row.TenantID), strings.TrimSpace(row.ID), body.Page, body.Size)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PostPublicKeyBillingOrderStatus(c *gin.Context) {
	var body struct {
		APIKey  string `json:"api_key"`
		OrderNo string `json:"order_no"`
		Sync    bool   `json:"sync,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_billing_request", "message": "invalid JSON body"}})
		return
	}
	row, ok := h.resolvePublicBillingAPIKey(c, body.APIKey)
	if !ok {
		return
	}
	order, err := h.billingService().GetOrderForAPIKey(c.Request.Context(), strings.TrimSpace(row.TenantID), strings.TrimSpace(row.ID), body.OrderNo, body.Sync)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, order)
}

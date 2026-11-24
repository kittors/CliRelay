package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/billing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	log "github.com/sirupsen/logrus"
)

// SetBillingService overrides the billing service (used by unit tests).
func (h *Handler) SetBillingService(svc *billing.Service) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.billingSvc = svc
	h.mu.Unlock()
}

func (h *Handler) billingService() *billing.Service {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.billingSvc != nil {
		svc := h.billingSvc
		h.mu.Unlock()
		return svc
	}
	h.mu.Unlock()

	db := usage.RuntimeDB()
	store := billing.NewStore(db)
	if db != nil && billing.DefaultStore() == nil {
		billing.SetDefaultStore(store)
	}
	svc := billing.NewService(billing.ServiceOptions{
		Store:          store,
		DailyReset:     h.billingDailyReset,
		UsageLookup:    billingLifetimeUsageLookup,
		OnQuotaChanged: h.refreshAPIKeyCache,
	})
	h.mu.Lock()
	if h.billingSvc == nil {
		h.billingSvc = svc
	} else {
		svc = h.billingSvc
	}
	h.mu.Unlock()
	return svc
}

func (h *Handler) billingDailyReset(_ context.Context, tenantID string, kind billing.TargetKind, targetID, orderNo string) error {
	actor := usage.PeriodSpendingResetActor{
		UserID:   "billing:" + orderNo,
		Username: "billing",
		Kind:     "billing",
	}
	if kind == billing.TargetKindEndUser {
		reset, err := usage.ResetPeriodSpendingByEndUserForTenant(tenantID, targetID, []quota.Period{quota.PeriodDay}, actor)
		if err != nil {
			return err
		}
		if err := usage.UpsertEndUserDailySpendingReset(tenantID, targetID, reset.Raw.Day); err != nil {
			return err
		}
		_ = usage.InsertEndUserDailySpendingResetEvent(usage.EndUserDailySpendingResetEvent{
			TenantID:            tenantID,
			EndUserID:           targetID,
			CostBaseline:        reset.Raw.Day,
			EffectiveUsedBefore: reset.EffectiveUsedBefore.Day,
			RawTodayCost:        reset.Raw.Day,
			ActorUserID:         actor.UserID,
			ActorUsername:       actor.Username,
			ActorKind:           actor.Kind,
		})
		return nil
	}
	_, err := h.apiKeySettingsForTenant(tenantID).ResetDailySpending(&targetID, nil, apikeysettings.DailySpendingResetActor{
		UserID:   actor.UserID,
		Username: actor.Username,
		Kind:     actor.Kind,
	})
	return err
}

func billingLifetimeUsageLookup(_ string, kind billing.TargetKind, targetID string) float64 {
	switch kind {
	case billing.TargetKindEndUser:
		used, err := usage.QueryTotalCostByEndUser(targetID)
		if err == nil {
			return used
		}
	case billing.TargetKindAPIKey:
		if row := usage.GetAPIKeyByID(targetID); row != nil {
			used, err := usage.QueryTotalCostByKey(row.Key)
			if err == nil {
				return used
			}
		}
	}
	return 0
}

func billingBaseURLFromRequest(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(c.Request.Host)
	}
	if host == "" {
		return ""
	}
	scheme := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + host
}

func respondBillingError(c *gin.Context, err error) {
	var fieldErr *billing.FieldError
	switch {
	case errors.As(err, &fieldErr):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"code":    "invalid_billing_request",
			"message": fieldErr.Error(),
			"details": gin.H{"field": fieldErr.Field},
		}})
	case errors.Is(err, billing.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "billing_unavailable", "message": err.Error()}})
	case errors.Is(err, billing.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "not_found", "message": err.Error()}})
	case errors.Is(err, billing.ErrDisabled):
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "billing_disabled", "message": err.Error()}})
	case errors.Is(err, billing.ErrChannelDisabled):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "channel_disabled", "message": err.Error()}})
	case errors.Is(err, billing.ErrInvalidSignature):
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"code": "invalid_signature", "message": err.Error()}})
	case errors.Is(err, billing.ErrAmountMismatch):
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "amount_mismatch", "message": err.Error()}})
	case errors.Is(err, billing.ErrInvalidTransition):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "invalid_order_state", "message": err.Error()}})
	case errors.Is(err, billing.ErrProviderFailed):
		// Log upstream payment gateway failures and return 424 Failed Dependency rather than 502:
		// Cloudflare intercepts origin HTTP 502 responses and replaces the JSON error body with
		// a generic Cloudflare 502 HTML page, hiding the real gateway error from the frontend.
		log.WithError(err).Warn("billing: payment provider request failed")
		c.JSON(http.StatusFailedDependency, gin.H{"error": gin.H{"code": "provider_failed", "message": err.Error()}})
	default:
		log.WithError(err).Warn("billing: request failed")
		writeError(c, http.StatusInternalServerError, "billing_failed", "计费操作处理失败，服务端内部发生异常，请稍后重试或查看服务端日志排查")
	}
}

func writeInvalidBillingBodyError(c *gin.Context, err error) {
	// Keep code "invalid_billing_request" for billing frontend error handlers while
	// providing detailed JSON decode diagnostics.
	var detailMsg string
	if err != nil {
		detailMsg = describeInvalidJSONBody(err)
	}
	if detailMsg == "" {
		detailMsg = "计费请求体 JSON 格式无效，请检查字段格式和数据类型后重试"
	}
	writeError(c, http.StatusBadRequest, "invalid_billing_request", detailMsg)
}

func (h *Handler) GetBillingSettings(c *gin.Context) {
	view, err := h.billingService().GetSettingsView(c.Request.Context(), effectiveTenantID(c), billingBaseURLFromRequest(c))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, view)
}

func (h *Handler) PutBillingSettings(c *gin.Context) {
	var patch billing.SettingsPatch
	if err := c.ShouldBindJSON(&patch); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	principal, _ := principalFromContext(c)
	view, err := h.billingService().UpdateSettings(c.Request.Context(), effectiveTenantID(c), principal.User.Username, billingBaseURLFromRequest(c), patch)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, view)
}

func (h *Handler) PostBillingSettingsTest(c *gin.Context) {
	var req billing.TestChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	result, err := h.billingService().TestChannel(c.Request.Context(), effectiveTenantID(c), req)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetBillingPlans(c *gin.Context) {
	items, err := h.billingService().ListPlans(c.Request.Context(), effectiveTenantID(c))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PostBillingPlan(c *gin.Context) {
	var input billing.PlanInput
	if err := c.ShouldBindJSON(&input); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	plan, err := h.billingService().CreatePlan(c.Request.Context(), effectiveTenantID(c), input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusCreated, plan)
}

func (h *Handler) PatchBillingPlan(c *gin.Context) {
	var input billing.PlanInput
	if err := c.ShouldBindJSON(&input); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	plan, err := h.billingService().UpdatePlan(c.Request.Context(), effectiveTenantID(c), c.Param("id"), input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, plan)
}

func (h *Handler) DeleteBillingPlan(c *gin.Context) {
	if err := h.billingService().DeletePlan(c.Request.Context(), effectiveTenantID(c), c.Param("id")); err != nil {
		respondBillingError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetBillingBindings(c *gin.Context) {
	items, err := h.billingService().ListBindingViews(c.Request.Context(), effectiveTenantID(c), billingBaseURLFromRequest(c))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) PutBillingBinding(c *gin.Context) {
	var input billing.BindingInput
	if err := c.ShouldBindJSON(&input); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	view, err := h.billingService().UpsertBinding(c.Request.Context(), effectiveTenantID(c), billingBaseURLFromRequest(c), input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) PostBillingBindingRotateCode(c *gin.Context) {
	view, err := h.billingService().RotateBindingPublicCode(c.Request.Context(), effectiveTenantID(c), c.Param("id"), billingBaseURLFromRequest(c))
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) DeleteBillingBinding(c *gin.Context) {
	if err := h.billingService().DeleteBinding(c.Request.Context(), effectiveTenantID(c), c.Param("id")); err != nil {
		respondBillingError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) PatchBillingEntitlement(c *gin.Context) {
	principal, _ := principalFromContext(c)
	tenantID := effectiveTenantID(c)
	var input billing.AdjustEntitlementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	view, err := h.billingService().AdjustEntitlement(c.Request.Context(), tenantID, input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	if audit := identity.Default(); audit != nil {
		audit.RecordAudit(c.Request.Context(), identity.AuditEvent{
			TenantID: tenantID, ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
			Action: "billing.entitlement.adjust", ResourceType: string(view.TargetKind), ResourceID: view.TargetID, Result: "success",
			Changes: map[string]any{"mode": input.Mode, "delta_days": input.DeltaDays, "expires_at": view.ExpiresAt},
		})
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) GetBillingOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	result, err := h.billingService().ListOrders(c.Request.Context(), effectiveTenantID(c), billing.OrderFilter{
		Status:     c.Query("status"),
		Search:     c.Query("search"),
		TargetKind: billing.TargetKind(strings.TrimSpace(c.Query("target_kind"))),
		TargetID:   c.Query("target_id"),
		Page:       page,
		Size:       size,
	})
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PostBillingOrderSync(c *gin.Context) {
	order, err := h.billingService().SyncOrderStatus(c.Request.Context(), effectiveTenantID(c), c.Param("order_no"), false)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, order)
}

func (h *Handler) PostBillingOrderFulfill(c *gin.Context) {
	principal, _ := principalFromContext(c)
	tenantID := effectiveTenantID(c)
	orderNo := c.Param("order_no")
	order, err := h.billingService().ManualFulfill(c.Request.Context(), tenantID, orderNo)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	if audit := identity.Default(); audit != nil {
		audit.RecordAudit(c.Request.Context(), identity.AuditEvent{
			TenantID: tenantID, ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
			Action: "billing.order.manual_fulfill", ResourceType: "billing_order", ResourceID: orderNo, Result: "success",
			Changes: map[string]any{"summary": order.FulfillmentSummary, "amount_cents": order.AmountCents},
		})
	}
	c.JSON(http.StatusOK, order)
}

func (h *Handler) PostBillingManualOrder(c *gin.Context) {
	principal, _ := principalFromContext(c)
	tenantID := effectiveTenantID(c)
	var input billing.ManualOrderInput
	if err := c.ShouldBindJSON(&input); err != nil {
		writeInvalidBillingBodyError(c, err)
		return
	}
	order, err := h.billingService().CreateManualOrder(c.Request.Context(), tenantID, principal.User.Username, input)
	if err != nil {
		respondBillingError(c, err)
		return
	}
	if audit := identity.Default(); audit != nil {
		audit.RecordAudit(c.Request.Context(), identity.AuditEvent{
			TenantID: tenantID, ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
			Action: "billing.order.manual_create", ResourceType: "billing_order", ResourceID: order.OrderNo, Result: "success",
			Changes: map[string]any{"target_kind": order.TargetKind, "target_id": order.TargetID, "amount_cents": order.AmountCents, "summary": order.FulfillmentSummary},
		})
	}
	c.JSON(http.StatusCreated, order)
}

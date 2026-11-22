package routes

import (
	"github.com/gin-gonic/gin"
	managementhandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
)

func registerManagementBillingRoutes(group *gin.RouterGroup, h *managementhandlers.Handler) {
	group.GET("/billing/settings", h.GetBillingSettings)
	group.PUT("/billing/settings", h.PutBillingSettings)
	group.POST("/billing/settings/test", h.PostBillingSettingsTest)
	group.GET("/billing/plans", h.GetBillingPlans)
	group.POST("/billing/plans", h.PostBillingPlan)
	group.PATCH("/billing/plans/:id", h.PatchBillingPlan)
	group.DELETE("/billing/plans/:id", h.DeleteBillingPlan)
	group.GET("/billing/bindings", h.GetBillingBindings)
	group.PUT("/billing/bindings", h.PutBillingBinding)
	group.POST("/billing/bindings/:id/rotate-code", h.PostBillingBindingRotateCode)
	group.DELETE("/billing/bindings/:id", h.DeleteBillingBinding)
	group.GET("/billing/orders", h.GetBillingOrders)
	group.POST("/billing/orders/:order_no/sync", h.PostBillingOrderSync)
	group.POST("/billing/orders/:order_no/fulfill", h.PostBillingOrderFulfill)
	group.POST("/billing/orders/manual", h.PostBillingManualOrder)
}

func registerPortalBillingRoutes(portal *gin.RouterGroup, h *managementhandlers.Handler) {
	portal.GET("/billing/overview", h.GetPortalBillingOverview)
	portal.POST("/billing/orders", h.PostPortalBillingOrder)
	portal.GET("/billing/orders", h.GetPortalBillingOrders)
	portal.GET("/billing/orders/:order_no", h.GetPortalBillingOrder)
}

func registerPublicBillingRoutes(pub *gin.RouterGroup, h *managementhandlers.Handler) {
	pub.GET("/billing/pay/:code", h.GetPublicBillingPay)
	pub.POST("/billing/pay/:code/orders", h.PostPublicBillingOrder)
	pub.GET("/billing/pay/:code/orders/:order_no", h.GetPublicBillingOrder)
	pub.POST("/billing/callback/xunhupay/:tenant_id/:channel", h.PostPublicXunhuPayCallback)
}

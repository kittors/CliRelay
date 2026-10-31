package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	log "github.com/sirupsen/logrus"
)

const tenantAccessRecheckInterval = 2 * time.Second

func withTenantAccessLease(parent context.Context, service *identity.Service, tenantID string, deadline *time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if deadline != nil {
		deadlineCtx, deadlineCancel := context.WithDeadline(ctx, *deadline)
		baseCancel := cancel
		ctx = deadlineCtx
		cancel = func() {
			deadlineCancel()
			baseCancel()
		}
	}
	go func() {
		ticker := time.NewTicker(tenantAccessRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := service.ValidateTenantAccess(ctx, tenantID); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func AuthMiddleware(manager *sdkaccess.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			diagnostics.SetLocalError(c, http.StatusInternalServerError, "local_auth", "auth_manager_unavailable", "server_error", "authentication manager not initialized")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "authentication manager not initialized"})
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result != nil {
				c.Set("apiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				apiKeyID := result.APIKeyID
				apiKeyName := result.APIKeyName
				if apiKeyID == "" {
					if keyIdentity := usage.ResolveAPIKeyIdentity(result.Principal); keyIdentity != nil {
						apiKeyID = keyIdentity.ID
						apiKeyName = keyIdentity.Name
					}
				}
				tenantID := result.TenantID
				if tenantID == "" {
					tenantID = usage.ResolveAPIKeyTenant(result.Principal)
				}
				if service := identity.Default(); service != nil && tenantID != "" {
					deadline, tenantErr := service.TenantAccessDeadline(c.Request.Context(), tenantID)
					if tenantErr != nil {
						diagnostics.SetLocalError(c, http.StatusForbidden, "local_auth", "tenant_unavailable", "authentication_error", tenantErr.Error())
						c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": tenantErr.Error()})
						return
					}
					if deadline != nil {
						leaseCtx, cancel := withTenantAccessLease(c.Request.Context(), service, tenantID, deadline)
						defer cancel()
						c.Request = c.Request.WithContext(leaseCtx)
					}
				}
				if tenantID != "" {
					c.Set("tenantID", tenantID)
				}
				diagnostics.SetAuth(c, result.Provider, result.Principal, apiKeyID, apiKeyName)
				if apiKeyID != "" {
					c.Set(apiKeyIDContextKey, apiKeyID)
				}
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			c.Next()
			return
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		diagnostics.SetLocalError(c, statusCode, "local_auth", "authentication_failed", "authentication_error", err.Message)
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}

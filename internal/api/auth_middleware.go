package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	log "github.com/sirupsen/logrus"
)

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
				apiKeyID := ""
				apiKeyName := ""
				if identity := usage.ResolveAPIKeyIdentity(result.Principal); identity != nil {
					apiKeyID = identity.ID
					apiKeyName = identity.Name
				}
				diagnostics.SetAuth(c, result.Provider, result.Principal, apiKeyID, apiKeyName)
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

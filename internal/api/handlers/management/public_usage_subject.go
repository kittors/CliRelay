package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

type publicUsageSubject struct {
	TenantID  string
	EndUserID string
	APIKey    string
	APIKeyRow *usage.APIKeyRow
}

func (s publicUsageSubject) logQueryParams(days int) usage.LogQueryParams {
	params := usage.LogQueryParams{TenantID: s.TenantID, Days: days}
	if strings.TrimSpace(s.EndUserID) != "" {
		params.EndUserID = s.EndUserID
	} else if strings.TrimSpace(s.APIKey) != "" {
		params.APIKeys = []string{s.APIKey}
	}
	return params
}

// resolvePublicUsageSubject binds portal requests to the authenticated end user.
// Raw-secret callers remain supported for CC Switch and legacy public lookup.
func (h *Handler) resolvePublicUsageSubject(c *gin.Context, apiKey string) (publicUsageSubject, bool) {
	if token := bearerToken(c); strings.HasPrefix(token, "cpt_") {
		user, _, ok := h.authenticatePortal(c)
		if !ok {
			return publicUsageSubject{}, false
		}
		return publicUsageSubject{
			TenantID:  strings.TrimSpace(user.TenantID),
			EndUserID: strings.TrimSpace(user.ID),
		}, true
	}

	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key parameter is required"})
		return publicUsageSubject{}, false
	}
	// Presenting the key is the only proof of ownership here, and the example keys
	// from config.example.yaml are public. Refuse them the way the client API does
	// rather than expose whatever was recorded under them.
	if config.IsPlaceholderAPIKey(apiKey) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid API key"})
		return publicUsageSubject{}, false
	}

	// Raw secret lookup is key-scoped only. Do not promote to account-wide via
	// end_user_id; portal cpt_ sessions above already use EndUserID without a secret.
	subject := publicUsageSubject{APIKey: apiKey}
	if row := usage.GetAPIKey(apiKey); row != nil {
		subject.APIKeyRow = row
		subject.TenantID = strings.TrimSpace(row.TenantID)
	} else {
		subject.TenantID = usage.ResolveAPIKeyTenant(apiKey)
	}
	return subject, true
}

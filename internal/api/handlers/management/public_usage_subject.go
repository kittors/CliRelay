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
		writeError(c, http.StatusBadRequest, "missing_api_key", "缺少必填参数 api_key（api_key parameter is required），请在请求体中提供待查询的 API Key")
		return publicUsageSubject{}, false
	}
	// Presenting the key is the only proof of ownership here, and the example keys
	// from config.example.yaml are public. Refuse them the way the client API does
	// rather than expose whatever was recorded under them.
	if config.IsPlaceholderAPIKey(apiKey) {
		// Keep "Invalid API key" verbatim for frontend ApiKeyLookupPage and public lookup tests.
		writeError(c, http.StatusUnauthorized, "invalid_api_key", "Invalid API key：该密钥为示例占位密钥，无法用于查询用量，请使用有效的正式 API Key")
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

package management

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

type oauthCallbackRequest struct {
	Provider    string `json:"provider"`
	RedirectURL string `json:"redirect_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
	Error       string `json:"error"`
}

func (h *Handler) PostOAuthCallback(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "code": "service_unavailable", "error": "OAuth 回调服务尚未初始化，请稍后重试"})
		return
	}

	var req oauthCallbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_request_body", "error": "OAuth 回调请求体 JSON 格式无效，请检查提交参数后重试"})
		return
	}

	canonicalProvider, err := NormalizeOAuthProvider(req.Provider)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "unsupported_provider", "error": "不支持的 OAuth 登录提供商，请确认 provider 参数正确"})
		return
	}

	state := strings.TrimSpace(req.State)
	code := strings.TrimSpace(req.Code)
	errMsg := strings.TrimSpace(req.Error)

	if rawRedirect := strings.TrimSpace(req.RedirectURL); rawRedirect != "" {
		u, errParse := url.Parse(rawRedirect)
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_redirect_url", "error": "提供的回调地址（redirect_url）格式无效，请粘贴完整的浏览器回调 URL"})
			return
		}
		q := u.Query()
		if state == "" {
			state = strings.TrimSpace(q.Get("state"))
		}
		if code == "" {
			code = strings.TrimSpace(q.Get("code"))
		}
		if errMsg == "" {
			errMsg = strings.TrimSpace(q.Get("error"))
			if errMsg == "" {
				errMsg = strings.TrimSpace(q.Get("error_description"))
			}
		}
	}

	if state == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "missing_oauth_state", "error": "缺少回调参数 state（state is required），请检查粘贴的回调 URL 是否完整"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_oauth_state", "error": "回调参数 state 格式无效（invalid state），请重新发起授权登录"})
		return
	}
	if code == "" && errMsg == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "missing_oauth_code", "error": "回调地址中缺少授权码 code 或上游错误参数（code or error is required），请检查回调 URL 是否完整"})
		return
	}

	sessionProvider, sessionTenantID, sessionStatus, ok := GetOAuthSessionWithTenant(state)
	if !ok {
		if sharedOAuthStateSuperseded(c, state) {
			c.JSON(http.StatusConflict, gin.H{"status": "error", "error": oauthSupersededMessage, "code": oauthCodeSessionSuperseded})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "OAuth 登录会话不存在或已过期（unknown or expired state），请重新发起授权登录", "code": oauthCodeSessionExpired})
		return
	}
	if sessionStatus == oauthSessionStatusCompleted {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "already_processed": true})
		return
	}
	if sessionStatus == oauthSessionStatusSuperseded {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": oauthSupersededMessage, "code": oauthCodeSessionSuperseded})
		return
	}
	if sessionStatus != "" {
		c.JSON(http.StatusConflict, gin.H{"status": "error", "error": sessionStatus})
		return
	}
	if !strings.EqualFold(sessionProvider, canonicalProvider) {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "oauth_provider_mismatch", "error": "回调提供商与当前 OAuth 登录状态不匹配（provider does not match state），请确认粘贴了对应平台的回调地址"})
		return
	}
	if sessionTenantID != "" && sessionTenantID != effectiveTenantID(c) {
		c.JSON(http.StatusForbidden, gin.H{"status": "error", "code": "oauth_tenant_mismatch", "error": "该 OAuth 登录状态属于其他租户工作区，无法在当前工作区完成回调"})
		return
	}
	if sessionTenantID != "" {
		if err = h.identity().ValidateTenantAccess(c.Request.Context(), sessionTenantID); err != nil {
			identityError(c, err)
			return
		}
	}

	if _, errWrite := WriteOAuthCallbackFileForPendingSession(h.cfg.AuthDir, canonicalProvider, state, code, errMsg); errWrite != nil {
		if errors.Is(errWrite, errOAuthSessionNotPending) {
			_, latestStatus, okLatest := GetOAuthSession(state)
			if okLatest && latestStatus == oauthSessionStatusCompleted {
				c.JSON(http.StatusOK, gin.H{"status": "ok", "already_processed": true})
				return
			}
			if okLatest && latestStatus == oauthSessionStatusSuperseded {
				c.JSON(http.StatusConflict, gin.H{"status": "error", "error": oauthSupersededMessage, "code": oauthCodeSessionSuperseded})
				return
			}
			if okLatest && latestStatus != "" {
				c.JSON(http.StatusConflict, gin.H{"status": "error", "error": latestStatus})
				return
			}
			c.JSON(http.StatusConflict, gin.H{"status": "error", "code": "oauth_flow_not_pending", "error": "当前 OAuth 登录流程已结束或不再处于等待回调状态，请重新发起登录"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "code": "oauth_callback_persist_failed", "error": "写入 OAuth 回调凭据文件失败，请检查服务器存储目录权限后重试"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

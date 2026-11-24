package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var (
	codexResetCreditsListURL    = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	codexResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

const codexResetCreditResponseLimit int64 = 1 << 20

type authFileResetCreditRequest struct {
	AuthIndexSnake       *string `json:"auth_index"`
	AuthIndexCamel       *string `json:"authIndex"`
	AuthIndexPascal      *string `json:"AuthIndex"`
	ID                   *string `json:"id"`
	Name                 *string `json:"name"`
	CreditIDSnake        *string `json:"credit_id"`
	CreditIDCamel        *string `json:"creditId"`
	RedeemRequestIDSnake *string `json:"redeem_request_id"`
	RedeemRequestIDCamel *string `json:"redeemRequestId"`
}

type codexResetCreditCandidate struct {
	ID        string
	ExpiresAt string
	ExpiresMS int64
	HasExpiry bool
}

// PostAuthFileResetCredit consumes one rate-limit reset credit for a Codex
// credential on the backend, using the credential's configured egress proxy
// and TLS fingerprint transport rather than routing through the generic
// /v0/management/api-call SSRF-guarded tool.
func (h *Handler) PostAuthFileResetCredit(c *gin.Context) {
	if h == nil || h.authManager == nil {
		writeServiceUnavailableError(c, "凭据管理服务")
		return
	}

	var body authFileResetCreditRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidBodyError(c, err)
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	authID := firstNonEmptyString(body.ID)
	authName := firstNonEmptyString(body.Name)
	if authIndex == "" && authID == "" && authName == "" {
		writeMissingFieldError(c, "auth_index", "请提供目标 Codex 账号的 auth_index、id 或 name")
		return
	}

	tenantID := effectiveTenantID(c)
	auth := h.findAuthForTenant(tenantID, authIndex, authID, authName)
	if auth == nil {
		identifier := firstNonEmptyValue(authIndex, authID, authName)
		writeError(c, http.StatusNotFound, "auth_not_found",
			fmt.Sprintf("未找到指定的账号凭据（%s），可能已被删除、刷新或不属于当前租户工作区", identifier),
			map[string]any{"identifier": identifier},
		)
		return
	}

	provider := resolveResetCreditProvider(auth)
	if provider != "codex" {
		displayName := firstNonEmptyValue(auth.FileName, auth.ID, auth.Index)
		writeError(c, http.StatusBadRequest, "unsupported_provider",
			fmt.Sprintf("额度重置仅支持 Codex 账号，当前凭据 %q 的类型为 %q", displayName, provider),
			map[string]any{"provider": provider, "auth_id": auth.ID},
		)
		return
	}

	ctx := c.Request.Context()
	svc := h.APITools().serviceForTenant(tenantID)
	token, tokenErr := svc.ResolveTokenForAuth(ctx, auth)
	token = strings.TrimSpace(token)
	if token == "" {
		if tokenErr != nil {
			reason := managementapitools.SanitizeErrorText(tokenErr.Error())
			log.WithFields(log.Fields{
				"tenant_id":  tenantID,
				"auth_id":    auth.ID,
				"auth_index": auth.EnsureIndex(),
			}).WithError(tokenErr).Warn("codex reset-credit token refresh failed")
			msg := "刷新该 Codex 账号的访问令牌（Token）失败，请检查账号授权状态或代理配置，必要时重新登录授权"
			if reason != "" {
				msg = fmt.Sprintf("刷新该 Codex 账号的访问令牌（Token）失败（%s），请检查账号授权状态或代理配置，必要时重新登录授权", reason)
			}
			writeError(c, http.StatusBadRequest, "auth_token_refresh_failed", msg)
			return
		}
		writeError(c, http.StatusBadRequest, "auth_token_not_found",
			"该 Codex 账号凭据中缺少可用的访问令牌（Token），请检查凭据内容是否完整或重新登录导入",
		)
		return
	}

	accountID := resolveCodexResetCreditAccountID(auth)
	targetCreditID := firstNonEmptyString(body.CreditIDSnake, body.CreditIDCamel)
	if targetCreditID == "" {
		if candidateID, err := fetchEarliestCodexResetCreditID(ctx, svc, auth, token, accountID); err != nil {
			log.WithFields(log.Fields{
				"tenant_id":  tenantID,
				"auth_id":    auth.ID,
				"auth_index": auth.EnsureIndex(),
			}).WithError(err).Warn("codex reset-credit candidate lookup failed; falling back to untargeted consume")
		} else if candidateID != "" {
			targetCreditID = candidateID
		}
	}

	redeemRequestID := firstNonEmptyString(body.RedeemRequestIDSnake, body.RedeemRequestIDCamel)
	if redeemRequestID == "" {
		redeemRequestID = uuid.NewString()
	}

	consumePayload := map[string]string{
		"redeem_request_id": redeemRequestID,
	}
	if targetCreditID != "" {
		consumePayload["credit_id"] = targetCreditID
	}
	payloadBytes, err := json.Marshal(consumePayload)
	if err != nil {
		writeInternalError(c, "构造 Codex 重置额度请求", err)
		return
	}

	upstreamStatus, respBody, err := doCodexResetCreditHTTP(ctx, svc, auth, http.MethodPost, codexResetCreditsConsumeURL, token, accountID, payloadBytes)
	if err != nil {
		writeUpstreamError(c, http.StatusBadGateway, "重置 Codex 账号额度", err)
		return
	}

	if upstreamStatus < http.StatusOK || upstreamStatus >= http.StatusMultipleChoices {
		writeCodexResetCreditUpstreamError(c, tenantID, auth, upstreamStatus, respBody)
		return
	}

	quotaStatusCleared, clearErr := h.authManager.ClearQuotaStatus(ctx, auth.ID)
	if clearErr != nil {
		log.WithFields(log.Fields{
			"tenant_id":  tenantID,
			"auth_id":    auth.ID,
			"auth_index": auth.EnsureIndex(),
		}).WithError(clearErr).Warn("codex reset-credit succeeded upstream, but failed to clear local quota status")
	}

	authSubjectID := ""
	if subj := usage.ResolveAuthSubjectIdentity(auth); subj != nil {
		authSubjectID = subj.ID
	}
	h.invalidateAIAccountCaches(tenantID, auth.EnsureIndex(), authSubjectID)

	resp := gin.H{
		"status":               "ok",
		"auth_index":           auth.EnsureIndex(),
		"auth_id":              auth.ID,
		"redeem_request_id":    redeemRequestID,
		"quota_status_cleared": quotaStatusCleared,
		"upstream_status":      upstreamStatus,
	}
	if targetCreditID != "" {
		resp["credit_id"] = targetCreditID
	}
	if len(respBody) > 0 {
		var parsed any
		if json.Unmarshal(respBody, &parsed) == nil && parsed != nil {
			resp["upstream"] = parsed
		}
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) findAuthForTenant(tenantID, authIndex, authID, authName string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.ListForTenant(tenantID)
	if authIndex != "" {
		for _, auth := range auths {
			if auth == nil {
				continue
			}
			if auth.EnsureIndex() == authIndex {
				return auth
			}
		}
		return nil
	}
	if authID != "" {
		for _, auth := range auths {
			if auth != nil && strings.TrimSpace(auth.ID) == authID {
				return auth
			}
		}
		return nil
	}
	if authName != "" {
		for _, auth := range auths {
			if auth == nil {
				continue
			}
			if strings.TrimSpace(auth.FileName) == authName || strings.TrimSpace(auth.ID) == authName {
				return auth
			}
		}
	}
	return nil
}

func resolveResetCreditProvider(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "" && auth.Metadata != nil {
		if rawType, ok := auth.Metadata["type"].(string); ok {
			provider = strings.ToLower(strings.TrimSpace(rawType))
		}
	}
	return provider
}

func resolveCodexResetCreditAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	keys := []string{"account_id", "accountId", "chatgpt_account_id", "chatgptAccountId"}
	if direct := lookupAuthMetadataOrAttr(auth, keys...); direct != "" {
		return direct
	}
	idToken := lookupAuthMetadataOrAttr(auth, "id_token", "idToken")
	if idToken == "" {
		return ""
	}
	claims, err := codexauth.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID)
}

func lookupAuthMetadataOrAttr(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		for _, key := range keys {
			if raw, ok := auth.Metadata[key]; ok {
				if s, ok := raw.(string); ok {
					if trimmed := strings.TrimSpace(s); trimmed != "" {
						return trimmed
					}
				}
			}
		}
	}
	for _, key := range keys {
		if trimmed := strings.TrimSpace(auth.Attributes[key]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func buildCodexResetCreditHeaders(token, accountID string) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", config.DefaultCodexFingerprintUserAgent)
	headers.Set("Originator", "Codex Desktop")
	headers.Set("OpenAI-Beta", "codex-1")
	headers.Set("Accept", "application/json")
	headers.Set("Sec-Fetch-Site", "none")
	headers.Set("Sec-Fetch-Mode", "no-cors")
	headers.Set("Sec-Fetch-Dest", "empty")
	if strings.TrimSpace(accountID) != "" {
		headers.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}
	return headers
}

func doCodexResetCreditHTTP(
	ctx context.Context,
	svc *managementapitools.Service,
	auth *coreauth.Auth,
	method string,
	rawURL string,
	token string,
	accountID string,
	body []byte,
) (int, []byte, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header = buildCodexResetCreditHeaders(token, accountID)

	client := util.NewHTTPClient(30 * time.Second)
	if transport := svc.APICallTransportForURL(auth, parsedURL); transport != nil {
		client.Transport = transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.WithError(closeErr).Debug("codex reset-credit response body close failed")
		}
	}()
	rawResp, err := bodyutil.ReadAll(resp.Body, codexResetCreditResponseLimit)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, rawResp, nil
}

func fetchEarliestCodexResetCreditID(
	ctx context.Context,
	svc *managementapitools.Service,
	auth *coreauth.Auth,
	token string,
	accountID string,
) (string, error) {
	status, rawResp, err := doCodexResetCreditHTTP(ctx, svc, auth, http.MethodGet, codexResetCreditsListURL, token, accountID, nil)
	if err != nil {
		return "", err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return "", fmt.Errorf("upstream status %d", status)
	}
	candidates := parseCodexResetCreditCandidates(rawResp)
	for _, candidate := range candidates {
		if candidate.ID != "" {
			return candidate.ID, nil
		}
	}
	return "", nil
}

func parseCodexResetCreditCandidates(raw []byte) []codexResetCreditCandidate {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed == nil {
		return nil
	}
	rawList := extractCodexResetCreditList(parsed, 0)
	if len(rawList) == 0 {
		return nil
	}
	candidates := make([]codexResetCreditCandidate, 0, len(rawList))
	for _, entry := range rawList {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if resetType := firstMapString(item, "reset_type", "resetType"); resetType != "" && !strings.EqualFold(resetType, "codex_rate_limits") {
			continue
		}
		if status := firstMapString(item, "status"); status != "" && !strings.EqualFold(status, "available") {
			continue
		}
		id := firstMapString(item, "id", "credit_id", "creditId")
		expiresAt := firstMapString(item, "expires_at", "expiresAt")
		candidate := codexResetCreditCandidate{
			ID:        id,
			ExpiresAt: expiresAt,
		}
		if expiresAt != "" {
			if ts, ok := parseCreditExpiryTime(expiresAt); ok {
				candidate.ExpiresMS = ts.UnixMilli()
				candidate.HasExpiry = true
			}
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].HasExpiry != candidates[j].HasExpiry {
			return candidates[i].HasExpiry
		}
		if !candidates[i].HasExpiry {
			return false
		}
		return candidates[i].ExpiresMS < candidates[j].ExpiresMS
	})
	return candidates
}

func extractCodexResetCreditList(payload any, depth int) []any {
	if list, ok := payload.([]any); ok {
		return list
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"credits", "rate_limit_reset_credits", "rateLimitResetCredits", "items", "data"} {
		val, exists := m[key]
		if !exists || val == nil {
			continue
		}
		if list, ok := val.([]any); ok && len(list) > 0 {
			return list
		}
		if depth < 1 {
			if nested := extractCodexResetCreditList(val, depth+1); len(nested) > 0 {
				return nested
			}
		}
	}
	return nil
}

func firstMapString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := m[key]; ok {
			if s, ok := raw.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func parseCreditExpiryTime(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func writeCodexResetCreditUpstreamError(c *gin.Context, tenantID string, auth *coreauth.Auth, upstreamStatus int, respBody []byte) {
	summary := managementapitools.ExtractUpstreamErrorSummary(respBody)
	lower := strings.ToLower(summary)
	details := map[string]any{
		"upstream_status": upstreamStatus,
	}
	if summary != "" {
		details["upstream_message"] = summary
	}

	log.WithFields(log.Fields{
		"tenant_id":       tenantID,
		"auth_id":         auth.ID,
		"auth_index":      auth.EnsureIndex(),
		"upstream_status": upstreamStatus,
		"upstream_error":  summary,
	}).Warn("codex reset-credit upstream rejected request")

	// Never mirror upstream 401 back to the management panel as HTTP 401: the
	// panel treats HTTP 401 from /v0/management/* as management session expiry
	// and signs the operator out.
	switch {
	case upstreamStatus == http.StatusUnauthorized:
		msg := "该 Codex 账号的上游登录态已失效（ChatGPT 返回 HTTP 401），请刷新令牌或重新登录授权后再试"
		if summary != "" {
			msg = fmt.Sprintf("该 Codex 账号的上游登录态已失效（ChatGPT 返回 HTTP 401：%s），请刷新令牌或重新登录授权后再试", summary)
		}
		writeError(c, http.StatusBadGateway, "upstream_auth_expired", msg, details)
	case upstreamStatus == http.StatusForbidden:
		msg := "ChatGPT 拒绝了本次额度重置请求（HTTP 403），可能是账号受限、风控拦截或当前出口 IP/代理被拒，请更换代理或检查账号状态"
		if summary != "" {
			msg = fmt.Sprintf("ChatGPT 拒绝了本次额度重置请求（HTTP 403：%s），可能是账号受限、风控拦截或当前出口 IP/代理被拒，请更换代理或检查账号状态", summary)
		}
		writeError(c, http.StatusBadGateway, "upstream_forbidden", msg, details)
	case upstreamStatus == http.StatusTooManyRequests:
		msg := "ChatGPT 上游触发频率限制（HTTP 429），请稍后再试"
		if summary != "" {
			msg = fmt.Sprintf("ChatGPT 上游触发频率限制（HTTP 429：%s），请稍后再试", summary)
		}
		writeError(c, http.StatusTooManyRequests, "upstream_rate_limited", msg, details)
	case upstreamStatus >= http.StatusInternalServerError:
		msg := fmt.Sprintf("ChatGPT 上游服务暂时异常（HTTP %d），请稍后重试", upstreamStatus)
		if summary != "" {
			msg = fmt.Sprintf("ChatGPT 上游服务暂时异常（HTTP %d：%s），请稍后重试", upstreamStatus, summary)
		}
		writeError(c, http.StatusBadGateway, "upstream_server_error", msg, details)
	case isNoResetCreditMessage(lower):
		msg := "该 Codex 账号当前没有可用的额度重置次数，请刷新账号状态确认剩余重置次数"
		if summary != "" {
			msg = fmt.Sprintf("该 Codex 账号当前没有可用的额度重置次数（%s），请刷新账号状态确认剩余重置次数", summary)
		}
		writeError(c, http.StatusConflict, "no_reset_credits_available", msg, details)
	default:
		msg := fmt.Sprintf("ChatGPT 拒绝了额度重置请求（HTTP %d），请刷新账号状态确认该账号是否满足重置条件", upstreamStatus)
		if summary != "" {
			msg = fmt.Sprintf("ChatGPT 拒绝了额度重置请求（HTTP %d：%s），请刷新账号状态确认该账号是否满足重置条件", upstreamStatus, summary)
		}
		writeError(c, http.StatusBadRequest, "reset_credit_rejected", msg, details)
	}
}

func isNoResetCreditMessage(lower string) bool {
	if lower == "" {
		return false
	}
	for _, needle := range []string{
		"no available",
		"no credit",
		"no reset credit",
		"insufficient",
		"not enough",
		"exhausted",
		"already redeemed",
		"already used",
		"already_used",
		"no_credits",
		"credit_not_found",
		"credit not found",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

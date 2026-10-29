package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/bodyutil"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/diagnostics"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/sjson"
)

// ─── Model policy ───────────────────────────────────────────────────────────
// 账号/Key 的模型路由与按模型额度只能在知道请求模型之后执行，所以单独成一个中间件，
// 挂在 QuotaMiddleware 之后、模型门控（modelRestrictionMiddleware）之前：
//
//  1. 读出请求的模型（OpenAI/Claude 在请求体的 model，Gemini 原生接口在路径里）；
//  2. 按路由改写模型，再检查命中的按模型规则，触顶且配置了降级模型时改用降级模型；
//  3. 结果不同于请求模型时改写请求，后续的模型门控、执行与记账都按改写后的模型进行，
//     计费与规则用量因此始终落在实际使用的模型上。
//
// Responses WebSocket 的升级请求不带模型，这里只发布逐轮使用的 gate。

// ModelPolicyMiddleware 返回模型策略中间件。effectiveModel 把客户端请求的模型映射成
// 网关实际路由用的模型（CC Switch 预设的模型映射），策略按映射后的模型判定。
func ModelPolicyMiddleware(effectiveModel func(*gin.Context, string) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey, _, ok := quotaCredentials(c)
		if !ok {
			c.Next()
			return
		}
		access := accesspolicy.Lookup(apiKey)
		if !access.HasModelPolicy() {
			c.Next()
			return
		}
		switch c.Request.Method {
		case http.MethodPost:
		case http.MethodGet:
			if IsWebsocketUpgrade(c.Request) {
				c.Set(handlers.ModelPolicyGateContextKey, modelPolicyTurnGate(access))
			}
			c.Next()
			return
		default:
			c.Next()
			return
		}

		requested, source, err := readRequestModel(c)
		if err != nil {
			if bodyutil.IsTooLarge(err) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
				return
			}
			if bodyutil.IsStorageUnavailable(err) {
				c.AbortWithStatusJSON(http.StatusInsufficientStorage, gin.H{"error": "request body temporary storage unavailable"})
				return
			}
			// 不是 JSON 或没有 model：交给后续处理，它们会给出协议相应的错误。
			c.Next()
			return
		}
		if requested == "" {
			c.Next()
			return
		}
		effective := requested
		if effectiveModel != nil {
			if mapped := strings.TrimSpace(effectiveModel(c, requested)); mapped != "" {
				effective = mapped
			}
		}
		decision, verdict := resolveModelPolicy(access, policyUsageFromContext(c, access), effective)
		if verdict != nil {
			verdict.abort(c)
			return
		}
		if decision.FinalModel != effective {
			if err := rewriteRequestModel(c, source, decision.FinalModel); err != nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"code": "invalid_request_body", "message": "request body could not be rewritten for the routed model",
				}})
				return
			}
			recordModelPolicy(c, requested, decision)
		}
		c.Next()
	}
}

// policyUsageFromContext 取 QuotaMiddleware 已经为本请求读到的用量快照；没有时新建。
func policyUsageFromContext(c *gin.Context, access *accesspolicy.Policy) *policyUsage {
	if value, ok := c.Get(policyUsageContextKey); ok {
		if usage, ok := value.(*policyUsage); ok && usage != nil && usage.policy == access {
			return usage
		}
	}
	usage := newPolicyUsage(access)
	c.Set(policyUsageContextKey, usage)
	return usage
}

// modelPolicyTurnGate 为 Responses WebSocket 的每一轮决议模型；每轮读取最新用量。
func modelPolicyTurnGate(access *accesspolicy.Policy) handlers.ModelPolicyGate {
	return func(model string) (string, *handlers.QuotaRejection) {
		model = strings.TrimSpace(model)
		if model == "" {
			return model, nil
		}
		decision, verdict := resolveModelPolicy(access, newPolicyUsage(access), model)
		if verdict != nil {
			return model, verdict.forTurn()
		}
		return decision.FinalModel, nil
	}
}

// resolveModelPolicy 按 Key 在前、账号在后的顺序检查按模型规则：Key 的规则更具体，
// 同时触顶时优先报告它。
func resolveModelPolicy(access *accesspolicy.Policy, usage *policyUsage, model string) (quota.ModelPolicyDecision, *quotaVerdict) {
	layers := make([]quota.ScopedRules, 0, 2)
	if len(access.KeyRules) > 0 {
		layers = append(layers, quota.ScopedRules{Scope: quota.RuleScopeKey, Rules: access.KeyRules,
			Usage: func(patterns []string) (quota.Matrix, error) {
				snapshot, err := usage.keySnapshot()
				if err != nil {
					return quota.Matrix{}, err
				}
				return snapshot.MatchedUsage(patterns), nil
			}})
	}
	if access.Owned() && len(access.AccountRules) > 0 {
		layers = append(layers, quota.ScopedRules{Scope: quota.RuleScopeAccount, Rules: access.AccountRules,
			Usage: func(patterns []string) (quota.Matrix, error) {
				snapshot, err := usage.accountSnapshot()
				if err != nil {
					return quota.Matrix{}, err
				}
				return snapshot.MatchedUsage(patterns), nil
			}})
	}
	decision, err := quota.ResolveModelPolicy(model, access.Routes, layers)
	if err != nil {
		return decision, quotaUsageUnavailableVerdict("model", "policy", access.TenantID, access.APIKeyID, err)
	}
	if decision.Exceeded != nil {
		return decision, modelRuleVerdict(decision.Exceeded, usage.fiveHourWindowStart(decision.Exceeded.Scope))
	}
	return decision, nil
}

type requestModelSource int

const (
	requestModelInBody requestModelSource = iota
	requestModelInGeminiPath
)

// readRequestModel 读出请求的模型，取法与模型门控一致：Gemini 原生接口
// （/v1beta/models/{model}:{method}）在路径里，其余在请求体的 model 字段。
// 请求体按流解码，不把落盘的大请求体整个读进内存。
func readRequestModel(c *gin.Context) (string, requestModelSource, error) {
	if action := c.Param("action"); action != "" {
		model, _, _ := strings.Cut(strings.TrimPrefix(action, "/"), ":")
		if model = strings.TrimSpace(model); model != "" {
			return model, requestModelInGeminiPath, nil
		}
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := bodyutil.DecodeJSONRequestBody(c, bodyutil.ModelRequestBodyLimit(), &body); err != nil {
		return "", requestModelInBody, err
	}
	return strings.TrimSpace(body.Model), requestModelInBody, nil
}

// rewriteRequestModel 把请求改写为 model。
func rewriteRequestModel(c *gin.Context, source requestModelSource, model string) error {
	if source == requestModelInGeminiPath {
		for i := range c.Params {
			if c.Params[i].Key != "action" {
				continue
			}
			_, method, found := strings.Cut(strings.TrimPrefix(c.Params[i].Value, "/"), ":")
			value := "/" + model
			if found {
				value += ":" + method
			}
			c.Params[i].Value = value
		}
		return nil
	}
	body, err := bodyutil.ReadRequestBody(c, bodyutil.ModelRequestBodyLimit())
	if err != nil {
		return err
	}
	rewritten, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return err
	}
	bodyutil.SetRequestBody(c, rewritten)
	return nil
}

// recordModelPolicy 把改写记进请求诊断，并用响应头告诉客户端实际使用的模型。
func recordModelPolicy(c *gin.Context, requested string, decision quota.ModelPolicyDecision) {
	snapshot := diagnostics.ModelPolicySnapshot{RequestedModel: requested, FinalModel: decision.FinalModel}
	if decision.Route != nil {
		snapshot.RouteID = decision.Route.ID
	}
	for _, step := range decision.Fallbacks {
		snapshot.FallbackRules = append(snapshot.FallbackRules, step.Exceeded.Rule.ID)
	}
	diagnostics.SetModelPolicy(c, snapshot)
	c.Header("X-CliRelay-Model-Requested", headerSafe(requested))
	c.Header("X-CliRelay-Model-Routed", headerSafe(decision.FinalModel))
	if len(decision.Fallbacks) > 0 {
		c.Header("X-CliRelay-Model-Fallback", headerSafe(decision.Fallbacks[len(decision.Fallbacks)-1].Exceeded.Rule.ID))
	}
}

// headerSafe 只保留可打印 ASCII：请求的模型名来自客户端，不能原样写进响应头。
func headerSafe(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
		if b.Len() >= 200 {
			break
		}
	}
	return b.String()
}

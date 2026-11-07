package handlers

import "github.com/gin-gonic/gin"

// ModelPolicyGate 决议一轮请求最终使用的模型：账号/Key 的模型路由与按模型额度
// （含超额降级）。返回应使用的模型（未改写时就是传入的模型），或拒绝。
//
// HTTP 请求在模型策略中间件里读请求体完成决议；Responses WebSocket 的每一轮在
// 帧里才带模型，中间件只能在升级请求上发布这个 gate，由帧处理逻辑逐轮调用。
// 每次调用都读取最新用量，同一轮内每个主体最多查一次库。
type ModelPolicyGate func(model string) (string, *QuotaRejection)

// ModelPolicyGateContextKey 是中间件存放 ModelPolicyGate 的 gin 上下文键。
const ModelPolicyGateContextKey = "cliproxy.modelPolicyGate"

// ModelPolicyGateFromGin 返回本请求发布的 gate；Key 没有模型策略时为 nil。
func ModelPolicyGateFromGin(c *gin.Context) ModelPolicyGate {
	if c == nil {
		return nil
	}
	value, ok := c.Get(ModelPolicyGateContextKey)
	if !ok {
		return nil
	}
	gate, _ := value.(ModelPolicyGate)
	return gate
}

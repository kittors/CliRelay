package diagnostics

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// ModelPolicySnapshot 记录账号/Key 的模型策略对本次请求做了什么：请求的模型、
// 路由与超额降级之后实际使用的模型。请求日志里的 model 是改写后的模型，没有这一段，
// 管理员看不出客户端原本请求的是什么。
type ModelPolicySnapshot struct {
	RequestedModel string   `json:"requested_model,omitempty"`
	FinalModel     string   `json:"final_model,omitempty"`
	RouteID        string   `json:"route_id,omitempty"`
	FallbackRules  []string `json:"fallback_rules,omitempty"`
}

// SetModelPolicy 记录模型策略的改写结果。
func SetModelPolicy(c *gin.Context, snapshot ModelPolicySnapshot) {
	d := FromGin(c)
	if d == nil {
		return
	}
	snapshot.RequestedModel = strings.TrimSpace(snapshot.RequestedModel)
	snapshot.FinalModel = strings.TrimSpace(snapshot.FinalModel)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ModelPolicy = cloneModelPolicy(&snapshot)
}

func cloneModelPolicy(in *ModelPolicySnapshot) *ModelPolicySnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.FallbackRules = append([]string(nil), in.FallbackRules...)
	return &out
}

package thinking

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyClaudeNativeEffort carries an OpenAI-style effort onto Claude adaptive
// thinking for models whose registry entry sets ThinkingSupport.AdaptiveEffort.
//
// body is the already-translated Claude request and original is the client
// request in sourceFormat. Only OpenAI Chat (reasoning_effort) and OpenAI
// Responses (reasoning.effort) sources are considered; native Claude requests,
// legacy models, custom models and efforts with no native equivalent are
// returned unchanged, so the existing budget translation still applies to them.
func ApplyClaudeNativeEffort(body, original []byte, model, sourceFormat string) []byte {
	var effort string
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "openai":
		effort = gjson.GetBytes(original, "reasoning_effort").String()
	case "openai-response":
		effort = gjson.GetBytes(original, "reasoning.effort").String()
	default:
		return body
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return body
	}
	info := registry.LookupModelInfo(ParseSuffix(model).ModelName, "claude")
	if info == nil || info.UserDefined || info.Thinking == nil || !info.Thinking.AdaptiveEffort {
		return body
	}
	native, ok := nativeClaudeEffort(effort, info.Thinking.Levels)
	if !ok {
		return body
	}
	body, _ = sjson.SetBytes(body, "thinking.type", "adaptive")
	body, _ = sjson.DeleteBytes(body, "thinking.budget_tokens")
	if native == "" {
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
	} else {
		body, _ = sjson.SetBytes(body, "output_config.effort", native)
	}
	return body
}

// nativeClaudeEffort maps a client effort onto the model's native effort levels.
// "auto" means adaptive thinking with the provider default effort (empty result).
// "minimal" has no native equivalent and uses the lowest native level.
// "none" and unknown values are not native and keep the budget translation.
func nativeClaudeEffort(effort string, levels []string) (string, bool) {
	switch effort {
	case "none":
		return "", false
	case "auto":
		return "", true
	case "minimal":
		if len(levels) == 0 {
			return "", false
		}
		return strings.ToLower(levels[0]), true
	}
	for _, level := range levels {
		if strings.EqualFold(level, effort) {
			return effort, true
		}
	}
	return "", false
}

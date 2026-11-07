package quota

import "strings"

// 模型规则与模型路由都按「模式」选中模型。这里是唯一的匹配定义，准入判定、
// 用量归集与面板预览都调用它，避免几处各写一份后对同一个模型名得出不同结论。

// NormalizeModelName 把模型名归一化为配额匹配用的形式：去首尾空白、转小写，
// 去掉客户端附加的思考强度后缀 "(high)" 与上下文窗口标记 "[1M]"。
//
// 后缀剥离与 internal/thinking.ParseSuffix 的规则一致（测试里有对照用例）。
// 这里不直接调用它：quota 是被网关、存储、管理端共同依赖的底层包，不应为了
// 一个字符串函数拖入模型注册表。同一模型带不同思考强度仍是同一个模型，
// 规则作者写的是基础模型名，配额也应按基础模型累计。
func NormalizeModelName(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = stripContextWindowMarker(model)
	if open := strings.LastIndex(model, "("); open > 0 && strings.HasSuffix(model, ")") {
		model = stripContextWindowMarker(strings.TrimSpace(model[:open]))
	}
	return model
}

// stripContextWindowMarker 去掉末尾形如 "[1m]"、"[128k]"、"[4096]" 的上下文窗口标记；
// 其它方括号内容（如 "[beta]"）是模型名本身的一部分，保持不动。
func stripContextWindowMarker(model string) string {
	open := strings.LastIndex(model, "[")
	if open <= 0 || !strings.HasSuffix(model, "]") {
		return model
	}
	content := model[open+1 : len(model)-1]
	i := 0
	for i < len(content) && content[i] >= '0' && content[i] <= '9' {
		i++
	}
	if i == 0 {
		return model
	}
	if i < len(content) && (content[i] == 'k' || content[i] == 'm') {
		i++
	}
	if i != len(content) {
		return model
	}
	return model[:open]
}

// bareModelName 去掉 "provider/" 形式的包装前缀。包装只是把请求路由到裸模型
// 的另一条通道（"ollama/gpt-oss:20b" 实际就是 gpt-oss:20b），配额上视为同一模型。
func bareModelName(model string) string {
	if idx := strings.Index(model, "/"); idx >= 0 {
		if bare := strings.TrimSpace(model[idx+1:]); bare != "" {
			return bare
		}
	}
	return model
}

// NormalizeModelPattern 归一化规则里写的模式：与模型名同样去空白、转小写、去后缀，
// 并把连续的 * 折叠成一个。
func NormalizeModelPattern(pattern string) string {
	pattern = NormalizeModelName(pattern)
	for strings.Contains(pattern, "**") {
		pattern = strings.ReplaceAll(pattern, "**", "*")
	}
	return pattern
}

// ModelPatternMatches 报告规则模式 pattern 是否命中模型 model。
//
//   - 不区分大小写，忽略思考强度后缀与上下文窗口标记；
//   - "*" 匹配任意长度的任意字符（包括 "/"），例如 "claude-opus-*"、"*opus*"；
//     单独一个 "*" 不被接受为规则模式（见 ValidateModelPattern），「全部模型」由
//     账号/Key 的总额度表达，不靠通配规则；
//   - 裸模型名命中它的所有包装："gpt-oss:20b" 命中 "ollama/gpt-oss:20b"。反过来不成立：
//     包装名只命中该包装本身，和「禁用模型」对包装的处理方向一致。
func ModelPatternMatches(pattern, model string) bool {
	pattern = NormalizeModelPattern(pattern)
	model = NormalizeModelName(model)
	if pattern == "" || model == "" {
		return false
	}
	if globMatch(pattern, model) {
		return true
	}
	if bare := bareModelName(model); bare != model {
		return globMatch(pattern, bare)
	}
	return false
}

// MatchesAnyModelPattern 报告 model 是否命中 patterns 中任意一个。
func MatchesAnyModelPattern(patterns []string, model string) bool {
	for _, pattern := range patterns {
		if ModelPatternMatches(pattern, model) {
			return true
		}
	}
	return false
}

// globMatch 实现只含 "*" 的通配匹配。贪心 + 回溯在模式里星号数量很少（规则模式
// 通常 0~2 个）时是线性的，不需要引入正则。
func globMatch(pattern, value string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	p, v := 0, 0
	star, mark := -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case p < len(pattern) && pattern[p] == value[v]:
			p++
			v++
		case star >= 0:
			p = star + 1
			mark++
			v = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

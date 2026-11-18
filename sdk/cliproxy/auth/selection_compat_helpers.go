package auth

import (
	"fmt"
	"strings"
)

func allowedChannelsFromMetadata(meta map[string]any) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta["allowed-channels"]
	if !ok || raw == nil {
		return nil
	}
	var values []string
	switch typed := raw.(type) {
	case string:
		values = strings.Split(typed, ",")
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	default:
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func authAllowedByChannels(auth *Auth, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return true
	}
	if auth == nil {
		return false
	}
	for _, identifier := range auth.ChannelIdentifiers() {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(identifier))]; ok {
			return true
		}
	}
	return false
}

func (m *Manager) selectorForRoutingScopeLocked(_ *runtimeConfigSnapshot, _ string, _ map[string]struct{}) Selector {
	if m != nil && m.selector != nil {
		return m.selector
	}
	return &RoundRobinSelector{}
}

type modelSuffixResult struct {
	ModelName string
	HasSuffix bool
	RawSuffix string
}

func isContextWindowSuffix(value string) bool {
	if value == "" {
		return false
	}
	index := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
	}
	if index == 0 {
		return false
	}
	if index < len(value) && value[index] != 'k' && value[index] != 'K' && value[index] != 'm' && value[index] != 'M' {
		return false
	}
	if index < len(value) {
		index++
	}
	return index == len(value)
}

func stripBracketSuffix(model string) string {
	lastOpen := strings.LastIndex(model, "[")
	if lastOpen < 0 || !strings.HasSuffix(model, "]") || lastOpen+1 >= len(model)-1 {
		return model
	}
	if !isContextWindowSuffix(model[lastOpen+1 : len(model)-1]) {
		return model
	}
	return model[:lastOpen]
}

func parseModelSuffix(model string) modelSuffixResult {
	model = stripBracketSuffix(model)
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen < 0 || !strings.HasSuffix(model, ")") {
		return modelSuffixResult{ModelName: model}
	}
	modelName := stripBracketSuffix(model[:lastOpen])
	return modelSuffixResult{ModelName: modelName, HasSuffix: true, RawSuffix: model[lastOpen+1 : len(model)-1]}
}

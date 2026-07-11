package usage

import (
	"encoding/json"
	"strings"
)

// stripStoredRequestDetailBodies preserves diagnostic metadata while ensuring
// request/response bodies cannot be retained indirectly in detail_content when
// full body storage is disabled.
func stripStoredRequestDetailBodies(raw string) string {
	sanitized, _ := sanitizeStoredRequestDetailBodies(raw)
	return sanitized
}

func sanitizeStoredRequestDetailBodies(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return raw, false
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return "", true
	}
	stripExchangeField(detail, "upstream", "request_log")
	stripExchangeField(detail, "response", "upstream_log")
	data, err := json.Marshal(detail)
	if err != nil {
		return "", true
	}
	sanitized := string(data)
	return sanitized, sanitized != raw
}

func stripExchangeField(detail map[string]any, section, field string) {
	value, ok := detail[section].(map[string]any)
	if !ok {
		return
	}
	raw, ok := value[field].(string)
	if !ok || raw == "" {
		return
	}
	value[field] = stripAPIExchangeBody(raw)
}

func stripAPIExchangeBody(raw string) string {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	skippingBody := false
	for _, line := range lines {
		if strings.HasPrefix(line, "=== API ") {
			skippingBody = false
		}
		if skippingBody {
			continue
		}
		out = append(out, line)
		if line == "Body:" {
			out = append(out, "<not stored>")
			skippingBody = true
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

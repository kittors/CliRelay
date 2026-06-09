package executor

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ensureTranslatedCodexModel(body []byte, fallback string) []byte {
	if strings.TrimSpace(gjson.GetBytes(body, "model").String()) != "" {
		return body
	}
	body, _ = sjson.SetBytes(body, "model", fallback)
	return body
}

func sanitizeCodexResponsesRequest(body []byte) []byte {
	for _, field := range []string{
		"max_output_tokens",
		"max_completion_tokens",
		"max_tokens",
	} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	return body
}

func extractCodexResponsesOutputItemDone(payload []byte) ([]byte, string, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return nil, "", false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Raw == "" {
		return nil, "", false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = item.Raw
	}
	return []byte(item.Raw), key, true
}

func mergeCodexResponsesCompletedOutput(payload []byte, pendingItems [][]byte, pendingKeys []string) []byte {
	if len(pendingItems) == 0 || gjson.GetBytes(payload, "type").String() != "response.completed" {
		return payload
	}

	output := gjson.GetBytes(payload, "response.output")
	merged := make([][]byte, 0, len(pendingItems)+len(output.Array()))
	seen := make(map[string]struct{}, len(pendingItems)+len(output.Array()))

	if output.IsArray() {
		for _, item := range output.Array() {
			raw := []byte(item.Raw)
			key := strings.TrimSpace(item.Get("id").String())
			if key == "" {
				key = item.Raw
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, raw)
		}
	}

	appended := false
	for idx, item := range pendingItems {
		key := pendingKeys[idx]
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
		appended = true
	}

	if !appended {
		return payload
	}

	buf := bytes.NewBuffer(make([]byte, 0, len(payload)+len(pendingItems)*64))
	buf.WriteByte('[')
	for idx, item := range merged {
		if idx > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(payload, "response.output", buf.Bytes())
	if err != nil {
		return payload
	}
	return updated
}

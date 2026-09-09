package responses

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexResponsesImageBridgeModel = "gpt-5.4-mini"

func ConvertOpenAIResponsesRequestToCodex(modelName string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = normalizeOpenAIResponsesImageRequest(rawJSON)

	// One-pass top-level mutate: avoid N full-document sjson copies on large bodies.
	sets := map[string][]byte{
		"stream":              util.JSONBool(true),
		"store":               util.JSONBool(false),
		"parallel_tool_calls": util.JSONBool(true),
		"include":             []byte(`["reasoning.encrypted_content"]`),
	}
	dels := []string{
		"max_output_tokens",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"service_tier",
		"truncation",
		"user",
		"context_management",
	}

	inputResult := gjson.GetBytes(rawJSON, "input")
	switch {
	case inputResult.Type == gjson.String:
		input, _ := sjson.Set(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`, "0.content.0.text", inputResult.String())
		sets["input"] = []byte(input)
	case inputResult.IsArray():
		if rewritten, ok := rewriteInputSystemRoles(inputResult); ok {
			sets["input"] = rewritten
		}
	}

	return util.MutateTopLevelObject(rawJSON, sets, dels)
}

func normalizeOpenAIResponsesImageRequest(rawJSON []byte) []byte {
	rawJSON = normalizeOpenAIResponsesPromptInput(rawJSON)

	requestModel := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	isImageOnlyModel := isOpenAIResponsesImageOnlyModel(requestModel)
	toolIndex := firstImageGenerationToolIndex(rawJSON)
	if toolIndex < 0 && !isImageOnlyModel {
		return rawJSON
	}

	// Image-tool path is rare and body is usually small; keep sjson here.
	if toolIndex < 0 {
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "tools", []byte(`[]`))
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "tools.-1", []byte(`{"type":"image_generation"}`))
		toolIndex = 0
	}

	toolModelPath := fmt.Sprintf("tools.%d.model", toolIndex)
	if isImageOnlyModel && strings.TrimSpace(gjson.GetBytes(rawJSON, toolModelPath).String()) == "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, toolModelPath, requestModel)
	}

	for _, field := range []string{
		"size",
		"quality",
		"background",
		"output_format",
		"output_compression",
		"moderation",
		"style",
		"partial_images",
	} {
		if !gjson.GetBytes(rawJSON, field).Exists() {
			continue
		}
		toolFieldPath := fmt.Sprintf("tools.%d.%s", toolIndex, field)
		if gjson.GetBytes(rawJSON, toolFieldPath).Exists() {
			rawJSON, _ = sjson.DeleteBytes(rawJSON, field)
			continue
		}
		rawJSON, _ = sjson.SetRawBytes(rawJSON, toolFieldPath, []byte(gjson.GetBytes(rawJSON, field).Raw))
		rawJSON, _ = sjson.DeleteBytes(rawJSON, field)
	}

	if isImageOnlyModel {
		if !gjson.GetBytes(rawJSON, "tool_choice").Exists() {
			rawJSON, _ = sjson.SetRawBytes(rawJSON, "tool_choice", []byte(`{"type":"image_generation"}`))
		}
		rawJSON, _ = sjson.SetBytes(rawJSON, "model", codexResponsesImageBridgeModel)
	}

	return rawJSON
}

func normalizeOpenAIResponsesPromptInput(rawJSON []byte) []byte {
	if gjson.GetBytes(rawJSON, "input").Exists() {
		return rawJSON
	}
	prompt := strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String())
	if prompt == "" {
		return rawJSON
	}
	return util.MutateTopLevelObject(rawJSON, map[string][]byte{
		"input": util.JSONString(prompt),
	}, []string{"prompt"})
}

func firstImageGenerationToolIndex(rawJSON []byte) int {
	tools := gjson.GetBytes(rawJSON, "tools")
	if !tools.IsArray() {
		return -1
	}
	for index, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			return index
		}
	}
	return -1
}

func isOpenAIResponsesImageOnlyModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-image-")
}

// rewriteInputSystemRoles rebuilds the input array once, converting role "system" -> "developer".
// Returns ok=false when no change is needed so callers can skip the top-level rewrite.
func rewriteInputSystemRoles(inputResult gjson.Result) ([]byte, bool) {
	if !inputResult.IsArray() {
		return nil, false
	}
	arr := inputResult.Array()
	changed := false
	for _, item := range arr {
		if item.Get("role").String() == "system" {
			changed = true
			break
		}
	}
	if !changed {
		return nil, false
	}

	var b bytes.Buffer
	b.Grow(len(inputResult.Raw))
	b.WriteByte('[')
	for i, item := range arr {
		if i > 0 {
			b.WriteByte(',')
		}
		if item.Get("role").String() != "system" {
			b.WriteString(item.Raw)
			continue
		}
		// Small object: one sjson on the item fragment only (not the whole request body).
		rewritten, err := sjson.Set(item.Raw, "role", "developer")
		if err != nil {
			b.WriteString(item.Raw)
			continue
		}
		b.WriteString(rewritten)
	}
	b.WriteByte(']')
	return b.Bytes(), true
}

// convertSystemRoleToDeveloper traverses the input array and converts any message items
// with role "system" to role "developer". This is necessary because Codex API does not
// accept "system" role in the input array.
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	rewritten, ok := rewriteInputSystemRoles(inputResult)
	if !ok {
		return rawJSON
	}
	return util.MutateTopLevelObject(rawJSON, map[string][]byte{"input": rewritten}, nil)
}

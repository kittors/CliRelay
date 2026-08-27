package executor

import (
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

	// Per-account opt-out switch, surfaced by the management API as
	// codex_image_generation_bridge. Absent means enabled.
	metadataKeyCodexImageGenerationBridge = "codex_image_generation_bridge"

	codexHostedImageGenerateTool = `{"type":"image_generation","action":"generate","model":"` + codexImageModel + `"}`
	codexHostedImageEditTool     = `{"type":"image_generation","action":"edit","model":"` + codexImageModel + `"}`

	// Sub2API-style guidance: hosted image_generation is intentional for clients that
	// cannot expose the local image_gen namespace (API-key custom providers).
	codexImageGenerationBridgeMarker = "<cliproxy-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace under custom/API-key providers; that does not mean image generation is unavailable. Do not claim the environment lacks image tooling solely because `image_gen` is absent, and do not ask the user to switch to CLI fallback as the primary fix.\n</cliproxy-codex-image-generation>"
)

var (
	// Hosted image_generation tool payload.
	//
	// The field set mirrors the request that /v1/images/generations already sends to
	// chatgpt.com/backend-api/codex/responses and that is verified to produce images
	// (see buildCodexImageResponsesRequest): `action` and `model` are both required there.
	// A bare {"type":"image_generation"} is accepted by the endpoint without an error, but
	// the model never invokes it, so image requests silently degrade into "I have no image
	// tool" text replies.
	imageGenToolJSON      = []byte(codexHostedImageGenerateTool)
	imageGenToolArrayJSON = []byte(`[` + codexHostedImageGenerateTool + `]`)

	// Same tool in edit mode, used when the current turn carries input images so the model
	// transforms what the user attached instead of generating an unrelated new picture.
	imageEditToolJSON      = []byte(codexHostedImageEditTool)
	imageEditToolArrayJSON = []byte(`[` + codexHostedImageEditTool + `]`)
)

// maybeEnsureCodexImageGenerationTool prepares outbound /responses tools for image gen.
//
// Policy (root-cause fix for Desktop):
//  1. If the client already advertises local image_gen (namespace/function), keep it and
//     strip hosted image_generation so Desktop uses /v1/images + disk save path.
//  2. Else if account bridge is enabled, inject hosted image_generation + bridge instructions.
//     API-key custom providers typically do not expose local image_gen, so hosted is required.
//  3. When image intent is present and only hosted tool is available, force
//     tool_choice=image_generation so the model cannot skip the tool and reply with text.
func maybeEnsureCodexImageGenerationTool(body []byte, auth *cliproxyauth.Auth, baseModel string, headers http.Header) []byte {
	if requestHasLocalImageGenTool(body) {
		return stripHostedImageGenerationTools(body)
	}
	if !codexImageGenerationBridgeEnabled(auth) {
		return body
	}
	intent := requestLooksLikeImageGenerationIntent(body)
	body = ensureCodexImageGenerationTool(body, baseModel, auth, headers, intent)
	if requestHasHostedImageGenerationTool(body) {
		body = ensureCodexImageGenerationBridgeInstructions(body)
		if intent {
			body = forceCodexImageGenerationToolChoice(body)
		}
	}
	return body
}

// requestCarriesImageInput reports whether the recent user turns attach images.
//
// Hosted image_generation takes an explicit action: "generate" invents a new picture and
// ignores what the user attached, "edit" transforms it. Picking generate for a turn that
// carries an image is how "make this logo blue" silently returns an unrelated new logo.
// Only recent turns count — an image from twenty turns ago is context, not the edit target.
func requestCarriesImageInput(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	items := input.Array()
	for i := len(items) - 1; i >= 0 && i >= len(items)-4; i-- {
		item := items[i]
		role := strings.TrimSpace(item.Get("role").String())
		if role != "" && role != "user" {
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			switch strings.TrimSpace(part.Get("type").String()) {
			case "input_image", "image_url":
				return true
			}
		}
	}
	return false
}

func requestLooksLikeImageGenerationIntent(body []byte) bool {
	// Explicit Image Gen skill / slash-command markers from Codex Desktop.
	markers := []string{
		"[$imagegen]",
		"<name>imagegen</name>",
		"skills/.system/imagegen",
		"$imagegen",
		"image_gen.imagegen",
		"built-in `image_gen`",
		"built-in image_gen",
	}
	// Prefer scanning recent user text, not the whole body (tools/schema can be huge).
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		lower := strings.ToLower(input.String())
		for _, m := range markers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return true
			}
		}
		return looksLikeChineseImageRequest(lower) || looksLikeEnglishImageRequest(lower)
	}
	if input.IsArray() {
		// Scan from the end: last user turn is decisive.
		items := input.Array()
		for i := len(items) - 1; i >= 0 && i >= len(items)-8; i-- {
			item := items[i]
			role := strings.TrimSpace(item.Get("role").String())
			if role != "" && role != "user" {
				continue
			}
			text := extractCodexInputItemText(item)
			if text == "" {
				continue
			}
			lower := strings.ToLower(text)
			for _, m := range markers {
				if strings.Contains(lower, strings.ToLower(m)) {
					return true
				}
			}
			if looksLikeChineseImageRequest(lower) || looksLikeEnglishImageRequest(lower) {
				return true
			}
		}
	}
	return false
}

func extractCodexInputItemText(item gjson.Result) string {
	if item.Get("content").Type == gjson.String {
		return item.Get("content").String()
	}
	content := item.Get("content")
	if !content.IsArray() {
		if t := item.Get("text"); t.Type == gjson.String {
			return t.String()
		}
		return ""
	}
	var b strings.Builder
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "input_text", "output_text", "text":
			if t := part.Get("text"); t.Type == gjson.String {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t.String())
			}
		}
	}
	return b.String()
}

func looksLikeChineseImageRequest(lower string) bool {
	// lower is already lowercased for ASCII; Chinese unchanged.
	keys := []string{"画一", "画个", "画只", "画一张", "生成一张", "生成图片", "生图", "出图", "改图", "修图", "绘一张"}
	for _, k := range keys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func looksLikeEnglishImageRequest(lower string) bool {
	keys := []string{
		"generate an image", "generate a image", "draw a ", "draw an ", "create an image",
		"make an image", "edit this image", "image generation", "generate image",
	}
	for _, k := range keys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func forceCodexImageGenerationToolChoice(body []byte) []byte {
	body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"image_generation"}`))
	return body
}

func requestHasLocalImageGenTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if isImageGenerationFunctionTool(tool) {
				return true
			}
		}
	}
	// Responses Lite embeds tools inside input additional_tools items.
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		nested := item.Get("tools")
		if !nested.IsArray() {
			continue
		}
		for _, tool := range nested.Array() {
			if isImageGenerationFunctionTool(tool) {
				return true
			}
		}
	}
	return false
}

func requestHasHostedImageGenerationTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			return true
		}
	}
	return false
}

// stripHostedImageGenerationTools removes Responses-native image_generation tools
// so the model prefers the client's local image_gen namespace when present.
func stripHostedImageGenerationTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		kept := make([]any, 0, len(tools.Array()))
		removed := false
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
				removed = true
				continue
			}
			kept = append(kept, tool.Value())
		}
		if removed {
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "tools")
			} else {
				body, _ = sjson.SetBytes(body, "tools", kept)
			}
		}
	}
	choiceType := strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String())
	if choiceType == "image_generation" {
		body, _ = sjson.SetBytes(body, "tool_choice", "auto")
	}
	return body
}

func ensureCodexImageGenerationBridgeInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if instructions.Exists() && instructions.Type == gjson.String {
		text := instructions.String()
		if strings.Contains(text, codexImageGenerationBridgeMarker) {
			return body
		}
		if strings.TrimSpace(text) == "" {
			body, _ = sjson.SetBytes(body, "instructions", codexImageGenerationBridgeText)
			return body
		}
		body, _ = sjson.SetBytes(body, "instructions", text+"\n\n"+codexImageGenerationBridgeText)
		return body
	}
	body, _ = sjson.SetBytes(body, "instructions", codexImageGenerationBridgeText)
	return body
}

// codexImageGenerationBridgeEnabled reports whether this account may receive the hosted tool.
//
// Codex clients never expose a local image_gen namespace to a custom provider, so without the
// bridge the Image Generation skill has nothing to call and the model answers that the session
// has no image tooling. Defaulting to off meant every account had to be flipped by hand for a
// documented Codex feature to work at all, so the bridge is on unless an operator turns it off.
func codexImageGenerationBridgeEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.Metadata == nil {
		return true
	}
	if raw, ok := auth.Metadata[metadataKeyCodexImageGenerationBridge]; ok {
		enabled, isBool := raw.(bool)
		if isBool {
			return enabled
		}
	}
	return true
}

func ensureCodexImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header, intent bool) []byte {
	if isCodexResponsesLiteRequest(body, headers) && !intent {
		// Responses Lite carries its tools inside input[].additional_tools, and a hosted tool
		// at the top level is not something Lite traffic is otherwise observed to send. Adding
		// it to every Lite turn would put all Lite conversations behind an unverified upstream
		// shape, so Lite only gets the tool on turns that actually ask for an image.
		return body
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(baseModel)), "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	toolJSON, toolArrayJSON := imageGenToolJSON, imageGenToolArrayJSON
	if requestCarriesImageInput(body) {
		toolJSON, toolArrayJSON = imageEditToolJSON, imageEditToolArrayJSON
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", toolArrayJSON)
		return body
	}
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(tool) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", toolJSON)
	return body
}

func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		name := tool.Get("name").String()
		return name == "image_gen.imagegen" || name == "imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		nested := tool.Get("tools")
		if !nested.IsArray() {
			return false
		}
		for _, nestedTool := range nested.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func isCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	if headers != nil && strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	value := gjson.GetBytes(body, codexResponsesLiteMetadata)
	if !value.Exists() {
		return false
	}
	return value.Type == gjson.True || (value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true"))
}

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

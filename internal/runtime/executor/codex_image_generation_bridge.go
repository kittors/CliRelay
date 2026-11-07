package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexcarrier"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

	// Per-account opt-out switch, surfaced by the management API as
	// codex_image_generation_bridge. Absent means enabled.
	metadataKeyCodexImageGenerationBridge = "codex_image_generation_bridge"
	// Per-account image model for the injected tool, surfaced by the management
	// API as codex_image_generation_model. Absent means the build default.
	metadataKeyCodexImageGenerationModel = "codex_image_generation_model"

	// Sub2API-style guidance: hosted image_generation is intentional for clients that
	// cannot expose the local image_gen namespace (API-key custom providers).
	codexImageGenerationBridgeMarker = "<cliproxy-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace under custom/API-key providers; that does not mean image generation is unavailable. Do not claim the environment lacks image tooling solely because `image_gen` is absent, and do not ask the user to switch to CLI fallback as the primary fix.\n</cliproxy-codex-image-generation>"
)

// codexHostedImageTool builds the hosted image_generation tool payload for one
// account.
//
// The field set mirrors the request that /v1/images/generations already sends to
// chatgpt.com/backend-api/codex/responses (see buildCodexImageResponsesRequest). A bare
// {"type":"image_generation"} is also invoked by the model, but it cannot express the edit
// action, and it leaves the image model up to whatever the backend defaults to — pinning
// both keeps the conversation path and the /v1/images endpoints on the same model.
//
// The model is per account rather than compiled in: Codex serves several
// gpt-image releases, and an operator comparing them had no way to move the
// conversation path off the default without editing the binary.
func codexHostedImageTool(action string, auth *cliproxyauth.Auth) []byte {
	tool := []byte(`{"type":"image_generation","action":"","model":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", codexBridgeImageModel(auth))
	return tool
}

func codexHostedImageToolArray(action string, auth *cliproxyauth.Auth) []byte {
	array := []byte(`[]`)
	array, _ = sjson.SetRawBytes(array, "0", codexHostedImageTool(action, auth))
	return array
}

// codexBridgeImageModel resolves which image model the injected tool asks for.
//
// A pinned value is validated against the Codex image catalog before it is used.
// The management API validates on write, but metadata also arrives from restored
// credential files and hand-edited JSON, and an unknown model here would turn
// every image turn on the account into an upstream 400 rather than falling back.
func codexBridgeImageModel(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return codexImageModel
	}
	pinned, _ := auth.Metadata[metadataKeyCodexImageGenerationModel].(string)
	pinned = strings.TrimSpace(pinned)
	if pinned == "" {
		return codexImageModel
	}
	for _, model := range registry.ListImageGenerationModelsForProvider(registry.ImageProviderCodex) {
		if strings.EqualFold(model.ID, pinned) {
			return model.ID
		}
	}
	return codexImageModel
}

// maybeEnsureCodexImageGenerationTool prepares outbound /responses tools for image gen.
//
// Policy (root-cause fix for Desktop):
//  1. If the client already advertises local image_gen (namespace/function), keep it and
//     strip hosted image_generation so Desktop uses /v1/images + disk save path.
//  2. Else if account bridge is enabled, inject hosted image_generation + bridge instructions.
//     API-key custom providers typically do not expose local image_gen, so hosted is required.
//     A turn that asks for no image but sets a reasoning effort gets no tool; see
//     ensureCodexImageGenerationTool.
//  3. Only an explicit imagegen invocation in the current user turn forces
//     tool_choice=image_generation, so the model cannot skip the tool and reply with text.
//     A keyword match is a guess and leaves the choice to the model.
//  4. A request that is going to call the hosted tool leaves without reasoning.effort, which
//     the backend would otherwise hand to the image model as complexity.
func maybeEnsureCodexImageGenerationTool(body []byte, auth *cliproxyauth.Auth, baseModel string, headers http.Header) []byte {
	body = repairCodexImageCarrierModel(body, baseModel)
	if requestHasLocalImageGenTool(body) {
		return stripHostedImageGenerationTools(body)
	}
	intent := classifyCodexImageGenerationIntent(body)
	if codexImageGenerationBridgeEnabled(auth) {
		body = ensureCodexImageGenerationTool(body, baseModel, auth, headers, intent)
		if requestHasHostedImageGenerationTool(body) {
			body = ensureCodexImageGenerationBridgeInstructions(body)
			if intent.forcesToolChoice() {
				body = forceCodexImageGenerationToolChoice(body)
			}
		}
	}
	return omitCodexReasoningEffortForImageCall(body, intent)
}

// codexRequestSetsReasoningEffort reports whether the request names a reasoning effort.
func codexRequestSetsReasoningEffort(body []byte) bool {
	effort := gjson.GetBytes(body, "reasoning.effort")
	return effort.Type == gjson.String && strings.TrimSpace(effort.String()) != ""
}

// omitCodexReasoningEffortForImageCall drops reasoning.effort from a request that is going to
// call the hosted image_generation tool: tool_choice is pinned to it, or the current turn asks
// for an image. Whoever attached the tool (the bridge, the gpt-image translator path or the
// client) does not matter.
//
// Since 2026-11-04 chatgpt.com/backend-api/codex/responses hands the outer reasoning.effort
// to the image call as its complexity, and the image model the Codex backend routes to
// (gpt-image-2-codex, whichever gpt-image model the tool names) rejects every value: production
// logged `Complexity '<effort>' is not supported for this image model.` for low, medium, high,
// xhigh and max on 2026-11-05, 170 failures across 16 sessions, each an HTTP 200 whose stream
// ends in image_generation_user_error and fails every retry the same way. Other relays hit by
// the same change found that the request succeeds once the effort is left out, which is also
// what the /v1/images path now sends. The rest of the reasoning object stays as the client
// sent it.
//
// Requests that will not call the tool keep their effort: lowering the reasoning of an
// ordinary coding turn because an image tool happens to be attached would be the wrong trade.
func omitCodexReasoningEffortForImageCall(body []byte, intent codexImageIntent) []byte {
	if !requestHasHostedImageGenerationTool(body) || !codexRequestSetsReasoningEffort(body) {
		return body
	}
	pinned := strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String()) == "image_generation"
	if !pinned && !intent.wantsImage() {
		return body
	}
	effort := gjson.GetBytes(body, "reasoning.effort").String()
	updated, err := sjson.DeleteBytes(body, "reasoning.effort")
	if err != nil {
		return body
	}
	if reasoning := gjson.GetBytes(updated, "reasoning"); reasoning.IsObject() && len(reasoning.Map()) == 0 {
		if trimmed, errDelete := sjson.DeleteBytes(updated, "reasoning"); errDelete == nil {
			updated = trimmed
		}
	}
	log.Debugf("codex image bridge: dropped reasoning.effort %q from a request that calls hosted image_generation", effort)
	return updated
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

func ensureCodexImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header, intent codexImageIntent) []byte {
	if isCodexResponsesLiteRequest(body, headers) && !intent.wantsImage() {
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
	if !intent.wantsImage() && codexRequestSetsReasoningEffort(body) {
		// A turn that asks for no image keeps its reasoning effort and goes without the tool.
		// The Codex backend hands any effort to the image call as complexity and the image model
		// rejects them all (see omitCodexReasoningEffortForImageCall), so a model that reached
		// for the tool unprompted would fail the whole turn; dropping the effort of an ordinary
		// coding turn to make room for a tool nobody asked for is the wrong trade. A turn that
		// does ask for an image gets the tool, and leaves without the effort.
		return body
	}

	action := "generate"
	if requestCarriesImageInput(body) {
		// The edit action transforms what the user attached; generate would invent
		// an unrelated picture and ignore it.
		action = "edit"
	}
	toolJSON := codexHostedImageTool(action, auth)
	toolArrayJSON := codexHostedImageToolArray(action, auth)

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

// repairCodexImageCarrierModel replaces the carrier model on a bridged image request.
//
// A request for an image-only model (gpt-image-*) cannot be sent as-is: Codex has
// no image endpoint on the OAuth path, so the translator rewrites it into a chat
// completion with an image_generation tool and puts a chat model at the top level
// to carry it. That carrier is a constant in internal/translator, which pull
// requests may not modify, and the constant now names a model upstream retired —
// every such request comes back "The '<model>' model is not supported when using
// Codex with a ChatGPT account."
//
// Correcting it here reaches all four outbound paths (streaming, non-streaming and
// both websocket variants) because they share this hook, and it keeps the fix on
// the executor side of that boundary. baseModel is the client's requested model,
// before translation, which is why it still reads gpt-image-* at this point.
func repairCodexImageCarrierModel(body []byte, baseModel string) []byte {
	if !isCodexImageOnlyModel(baseModel) {
		return body
	}
	carrier := strings.TrimSpace(codexcarrier.Resolve())
	if carrier == "" {
		return body
	}
	if strings.TrimSpace(gjson.GetBytes(body, "model").String()) == carrier {
		return body
	}
	updated, err := sjson.SetBytes(body, "model", carrier)
	if err != nil {
		return body
	}
	return updated
}

// isCodexImageOnlyModel reports whether a client asked for an image model directly,
// matching the prefix the translator uses to decide it must bridge the request.
func isCodexImageOnlyModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-image-")
}

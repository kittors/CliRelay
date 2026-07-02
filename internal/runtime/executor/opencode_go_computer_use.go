package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// mcpComputerUseFunctions defines the 10 function tools in the mcp__computer_use__
// namespace. These are vanilla OpenAI function tools — any model with function
// calling can invoke them. Codex Desktop routes the tool_call back to the local
// Computer Use MCP server which executes the actual screenshot / click / type
// operations on the client machine.
var mcpComputerUseFunctions = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__click",
			"description": "Click an element by index or pixel coordinates from screenshot. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":           map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"click_count":   map[string]any{"type": "integer", "description": "Number of clicks. Defaults to 1"},
					"element_index": map[string]any{"type": "string", "description": "Element index to click"},
					"mouse_button":  map[string]any{"type": "string", "description": "Mouse button to click. Defaults to left.", "enum": []string{"left", "right", "middle"}},
					"x":             map[string]any{"type": "number", "description": "X coordinate in screenshot pixel coordinates"},
					"y":             map[string]any{"type": "number", "description": "Y coordinate in screenshot pixel coordinates"},
				},
				"required":             []string{"app"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__drag",
			"description": "Drag from one point to another using pixel coordinates. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":    map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"from_x": map[string]any{"type": "number", "description": "Start X coordinate"},
					"from_y": map[string]any{"type": "number", "description": "Start Y coordinate"},
					"to_x":   map[string]any{"type": "number", "description": "End X coordinate"},
					"to_y":   map[string]any{"type": "number", "description": "End Y coordinate"},
				},
				"required":             []string{"app", "from_x", "from_y", "to_x", "to_y"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__get_app_state",
			"description": "Start an app use session if needed, then get the state of the app's key window and return a screenshot and accessibility tree. This must be called once per assistant turn before interacting with the app. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app": map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
				},
				"required":             []string{"app"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__list_apps",
			"description": "List the apps on this computer. Returns the set of apps that are currently running, as well as any that have been used in the last 14 days, including details on usage frequency. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__perform_secondary_action",
			"description": "Invoke a secondary accessibility action exposed by an element. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":        map[string]any{"type": "string", "description": "Secondary accessibility action name"},
					"app":           map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"element_index": map[string]any{"type": "string", "description": "Element identifier"},
				},
				"required":             []string{"app", "element_index", "action"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__press_key",
			"description": "Press a key or key-combination on the keyboard, including modifier and navigation keys.\n  - This supports xdotool's `key` syntax.\n  - Examples: \"a\", \"Return\", \"Tab\", \"super+c\", \"Up\", \"KP_0\" (for the numpad 0 key). This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app": map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"key": map[string]any{"type": "string", "description": "Key or key combination to press"},
				},
				"required":             []string{"app", "key"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__scroll",
			"description": "Scroll an element in a direction by a number of pages. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":           map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"direction":     map[string]any{"type": "string", "description": "Scroll direction: up, down, left, or right"},
					"element_index": map[string]any{"type": "string", "description": "Element identifier"},
					"pages":         map[string]any{"type": "number", "description": "Number of pages to scroll. Fractional values are supported. Defaults to 1"},
				},
				"required":             []string{"app", "element_index", "direction"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__select_text",
			"description": "Select text inside a text element, or place the text cursor before or after it. Provide text exactly as it appears in the accessibility tree, including any Markdown formatting. If the text is not unique, provide surrounding prefix or suffix text to disambiguate it. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":           map[string]any{"type": "string", "description": "App name or bundle identifier"},
					"element_index": map[string]any{"type": "string", "description": "Text element identifier"},
					"prefix":        map[string]any{"type": "string", "description": "Optional text immediately before the target, used to disambiguate repeated matches"},
					"selection":     map[string]any{"type": "string", "description": "Whether to select the text or place the cursor before or after it. Defaults to text.", "enum": []string{"text", "cursor_before", "cursor_after"}},
					"suffix":        map[string]any{"type": "string", "description": "Optional text immediately after the target, used to disambiguate repeated matches"},
					"text":          map[string]any{"type": "string", "description": "Target text as shown in the accessibility tree"},
				},
				"required":             []string{"app", "element_index", "text"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__set_value",
			"description": "Set the value of a settable accessibility element. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":           map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"element_index": map[string]any{"type": "string", "description": "Element identifier"},
					"value":         map[string]any{"type": "string", "description": "Value to assign"},
				},
				"required":             []string{"app", "element_index", "value"},
				"additionalProperties": false,
			},
		},
	},
	{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__computer_use__type_text",
			"description": "Type literal text using keyboard input. This tool is part of plugin `Computer Use`.",
			"strict":      false,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"app":  map[string]any{"type": "string", "description": "App name, full app path, or unambiguous bundle identifier"},
					"text": map[string]any{"type": "string", "description": "Literal text to type"},
				},
				"required":             []string{"app", "text"},
				"additionalProperties": false,
			},
		},
	},
}

var mcpNodeReplJSFunction = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "mcp__node_repl__js",
		"description": "Run JavaScript in a persistent Node-backed kernel with top-level await. This is the bridge used by the Browser and Chrome plugins.",
		"strict":      false,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code":       map[string]any{"type": "string", "description": "JavaScript source to execute in the persistent Node-backed kernel."},
				"timeout_ms": map[string]any{"type": "integer", "description": "Optional execution timeout in milliseconds. Defaults to 30000."},
				"title":      map[string]any{"type": "string", "description": "Short user-facing description of what this code block is doing."},
			},
			"required":             []string{"code"},
			"additionalProperties": false,
		},
	},
}

var mcpCodexToolBridgeFunctions = append(append([]map[string]any{}, mcpComputerUseFunctions...), mcpNodeReplJSFunction)

// opencodeGoInjectCodexToolBridgeTools checks whether the request payload
// already carries concrete Codex ecosystem MCP function definitions. When they
// are missing, it injects them so non-native models see Computer Use, Browser,
// and Chrome capabilities as regular function tools.
//
// The function detects whether existing tools are in OpenAI Chat Completions
// format ({"type":"function","function":{"name":"..."}}) or Claude /v1/messages
// format ({"name":"...","description":"...","input_schema":{...}}) and injects
// in the same format to prevent the translator from producing empty function
// names when the payload format differs from the injection format.
func opencodeGoInjectCodexToolBridgeTools(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}

	// Only inject when the request already has a tools array.
	tools := gjson.GetBytes(payload, "tools")
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0 {
		return payload
	}

	toolArray := tools.Array()

	// Detect format from the first concrete tool's structure.
	//   1. OpenAI Chat Completions: {"function":{"name":"..."}}
	//   2. Claude /v1/messages:     {"name":"...","input_schema":{...}} (no "function" key, no "type"="function")
	//   3. Responses API:           {"type":"function","name":"...","parameters":{...}} (no "function" key but has "type"="function")
	isClaudeFormat := false
	isResponsesAPIFormat := false
	for _, tool := range toolArray {
		if tool.Get("function").Exists() {
			// #1: OpenAI Chat Completions format
			isClaudeFormat = false
			isResponsesAPIFormat = false
			break
		} else if tool.Get("type").String() == "function" && tool.Get("name").Exists() {
			// #3: Responses API format - has top-level "type"="function" and "name"
			isClaudeFormat = false
			isResponsesAPIFormat = true
			break
		} else if tool.Get("type").String() == "namespace" {
			// Codex Responses API namespace tools need expansion for OpenAI-compatible upstreams.
			isClaudeFormat = false
			isResponsesAPIFormat = true
		} else if tool.Get("name").Exists() {
			// #2: Claude format - only has top-level "name" (no "type"="function")
			isClaudeFormat = true
			isResponsesAPIFormat = false
			break
		}
	}

	existingNames := map[string]struct{}{}
	for _, tool := range toolArray {
		var name string
		if isClaudeFormat {
			name = tool.Get("name").String()
		} else if isResponsesAPIFormat {
			name = tool.Get("name").String()
		} else {
			name = tool.Get("function.name").String()
		}
		if name != "" {
			existingNames[name] = struct{}{}
		}
	}

	missingFunctions := make([]map[string]any, 0, len(mcpCodexToolBridgeFunctions))
	for _, fn := range mcpCodexToolBridgeFunctions {
		name, _, _, ok := opencodeGoBridgeFunctionParts(fn)
		if !ok {
			continue
		}
		if _, exists := existingNames[name]; exists {
			continue
		}
		missingFunctions = append(missingFunctions, fn)
	}
	if len(missingFunctions) == 0 {
		return payload
	}

	// Inject each missing Codex bridge function tool into the tools array.
	startIdx := len(toolArray)

	if isClaudeFormat {
		// Inject in Claude /v1/messages format.
		for i, fn := range missingFunctions {
			name, desc, params, ok := opencodeGoBridgeFunctionParts(fn)
			if !ok {
				continue
			}

			claudeTool := fmt.Sprintf(`{"name":"%s","description":"","input_schema":null}`, name)
			claudeTool, _ = sjson.Set(claudeTool, "description", desc)

			paramsJSON, err := json.Marshal(params)
			if err != nil {
				break
			}
			claudeTool, _ = sjson.SetRaw(claudeTool, "input_schema", string(paramsJSON))

			path := fmt.Sprintf("tools.%d", startIdx+i)
			payload, err = sjson.SetRawBytes(payload, path, []byte(claudeTool))
			if err != nil {
				break
			}
		}
	} else if isResponsesAPIFormat {
		// Inject in Responses API format: {"type":"function","name":"...","description":"...","parameters":{...}}
		for i, fn := range missingFunctions {
			name, desc, params, ok := opencodeGoBridgeFunctionParts(fn)
			if !ok {
				continue
			}

			respTool := fmt.Sprintf(`{"type":"function","name":"%s","description":"","parameters":null}`, name)
			respTool, _ = sjson.Set(respTool, "description", desc)

			paramsJSON, err := json.Marshal(params)
			if err != nil {
				break
			}
			respTool, _ = sjson.SetRaw(respTool, "parameters", string(paramsJSON))

			path := fmt.Sprintf("tools.%d", startIdx+i)
			payload, err = sjson.SetRawBytes(payload, path, []byte(respTool))
			if err != nil {
				break
			}
		}
	} else {
		// Inject in OpenAI Chat Completions format (original behavior).
		for i, fn := range missingFunctions {
			path := fmt.Sprintf("tools.%d", startIdx+i)
			var err error
			payload, err = sjson.SetBytes(payload, path, fn)
			if err != nil {
				break
			}
		}
	}

	return payload
}

func opencodeGoBridgeFunctionParts(fn map[string]any) (string, string, any, bool) {
	fnMap, ok := fn["function"].(map[string]any)
	if !ok {
		return "", "", nil, false
	}
	name, ok := fnMap["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", "", nil, false
	}
	desc, _ := fnMap["description"].(string)
	params, ok := fnMap["parameters"]
	return name, desc, params, ok
}

// opencodeGoStripScreenshots removes base64 image data from tool result messages
// in the request payload. After the model processes a screenshot (via get_app_state),
// the raw image data is no longer needed and wastes large amounts of context tokens.
func opencodeGoStripScreenshots(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	payload = opencodeGoStripArrayScreenshots(payload, "messages")
	payload = opencodeGoStripArrayScreenshots(payload, "input")
	return payload
}

func opencodeGoStripArrayScreenshots(payload []byte, arrayPath string) []byte {
	arr := gjson.GetBytes(payload, arrayPath)
	if !arr.Exists() || !arr.IsArray() || len(arr.Array()) == 0 {
		return payload
	}
	modified := false
	items := arr.Array()
	for i, item := range items {
		if item.Get("role").String() != "tool" {
			continue
		}
		content := item.Get("content")
		if !content.Exists() {
			continue
		}
		path := fmt.Sprintf("%s.%d.content", arrayPath, i)
		if content.IsArray() {
			parts := content.Array()
			for j, part := range parts {
				partType := part.Get("type").String()
				if partType == "input_image" || partType == "image" || partType == "image_url" {
					partPath := fmt.Sprintf("%s.%d.content.%d", arrayPath, i, j)
					payload, _ = sjson.SetBytes(payload, partPath+".type", "text")
					payload, _ = sjson.SetBytes(payload, partPath+".text", "[Screenshot from previous turn]")
					_, _ = sjson.DeleteBytes(payload, partPath+".image_url")
					modified = true
				}
			}
		} else {
			text := content.String()
			if strings.HasPrefix(text, "data:image") || strings.HasPrefix(text, "[data:image") {
				payload, _ = sjson.SetBytes(payload, path, "[Screenshot from previous turn]")
				modified = true
			} else if len(text) > 1000 && strings.Contains(text, "base64") {
				payload, _ = sjson.SetBytes(payload, path, "[Screenshot from previous turn]")
				modified = true
			}
		}
	}
	if !modified {
		return payload
	}
	return payload
}

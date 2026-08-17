package responses

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

const responsesLiteToolsRequest = `{
	"model":"gpt-test",
	"input":[{
		"type":"additional_tools",
		"role":"developer",
		"tools":[
			{"type":"namespace","name":"functions","description":"","tools":[
				{"type":"custom","name":"exec","description":"Run code","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
				{"type":"function","name":"wait","description":"Wait","parameters":{"type":"object","properties":{"cell_id":{"type":"string"}},"required":["cell_id"],"additionalProperties":false},"strict":true}
			]},
			{"type":"namespace","name":"collaboration","description":"Team tools","tools":[
				{"type":"function","name":"spawn_agent","description":"Spawn","parameters":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false},"strict":true}
			]},
			{"type":"namespace","name":"mcp__node_repl__","description":"Node REPL","tools":[
				{"type":"function","name":"js","description":"Run JavaScript","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"mcp__computer_use__","description":"Computer use","tools":[
				{"type":"function","name":"list_apps","description":"List apps","parameters":{"type":"object"}}
			]}
		]
	}],
	"stream":true,
	"tool_choice":"auto"
}`

const responsesLiteFlatDefaultToolsRequest = `{
	"model":"gpt-test",
	"input":[{
		"type":"additional_tools",
		"tools":[
			{"type":"custom","name":"exec","description":"Run code"},
			{"type":"function","name":"wait","parameters":{"type":"object"}},
			{"type":"function","name":"request_user_input","parameters":{"type":"object"}},
			{"type":"namespace","name":"collaboration","tools":[
				{"type":"function","name":"list_agents","parameters":{"type":"object"}}
			]}
		]
	}]
}`

func TestConvertOpenAIResponsesRequestToChatCompletionsSupportsFlatDefaultTools(t *testing.T) {
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteFlatDefaultToolsRequest), false)
	wantNames := []string{"exec", "wait", "request_user_input", "collaboration__list_agents"}
	for i, want := range wantNames {
		if got := gjson.GetBytes(translated, fmt.Sprintf("tools.%d.function.name", i)).String(); got != want {
			t.Fatalf("tool %d name = %q, want %q: %s", i, got, want, translated)
		}
	}

	chatResponse := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"text(true)\"}"}},{"id":"call_wait","type":"function","function":{"name":"wait","arguments":"{\"cell_id\":\"1\"}"}}]}}]}`)
	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-test", []byte(responsesLiteFlatDefaultToolsRequest), translated, chatResponse, nil)
	if got := gjson.Get(out, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("output type = %q, want custom_tool_call: %s", got, out)
	}
	if got := gjson.Get(out, "output.0.name").String(); got != "exec" {
		t.Fatalf("output name = %q, want exec: %s", got, out)
	}
	if got := gjson.Get(out, "output.1.type").String(); got != "function_call" {
		t.Fatalf("function output type = %q, want function_call: %s", got, out)
	}
	if got := gjson.Get(out, "output.1.name").String(); got != "wait" {
		t.Fatalf("function output name = %q, want wait: %s", got, out)
	}
	if gjson.Get(out, "output.0.namespace").Exists() || gjson.Get(out, "output.1.namespace").Exists() {
		t.Fatalf("default namespace should be omitted: %s", out)
	}
}

func TestConvertChatCompletionsStreamRestoresFlatDefaultTools(t *testing.T) {
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteFlatDefaultToolsRequest), true)
	chunk := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"text(true)\"}"}},{"index":1,"id":"call_wait","type":"function","function":{"name":"wait","arguments":"{\"cell_id\":\"1\"}"}}]},"finish_reason":"tool_calls"}]}`)
	var state any
	events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-test", []byte(responsesLiteFlatDefaultToolsRequest), translated, chunk, &state)
	joined := strings.Join(events, "\n")

	if !strings.Contains(joined, `"type":"custom_tool_call"`) || !strings.Contains(joined, `"name":"exec"`) || !strings.Contains(joined, `"input":"text(true)"`) {
		t.Fatalf("stream did not restore flat custom tool call: %s", joined)
	}
	if !strings.Contains(joined, `"type":"function_call"`) || !strings.Contains(joined, `"name":"wait"`) {
		t.Fatalf("stream did not restore flat function call: %s", joined)
	}
	if strings.Contains(joined, `"namespace":"functions"`) {
		t.Fatalf("stream should omit the default namespace: %s", joined)
	}
}

func TestConvertOpenAIResponsesRequestToChatCompletionsFlattensResponsesLiteTools(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteToolsRequest), true)

	if got := gjson.GetBytes(out, "tools.#").Int(); got != 5 {
		t.Fatalf("tools count = %d, want 5: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.function.name").String(); got != "exec" {
		t.Fatalf("custom tool name = %q, want exec: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.0.function.parameters.required.0").String(); got != "input" {
		t.Fatalf("custom tool input parameter missing: %s", out)
	}
	if got := gjson.GetBytes(out, "tools.1.function.name").String(); got != "wait" {
		t.Fatalf("default namespace function name = %q, want wait: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.2.function.name").String(); got != "collaboration__spawn_agent" {
		t.Fatalf("namespaced function name = %q, want collaboration__spawn_agent: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.3.function.name").String(); got != "mcp__node_repl__js" {
		t.Fatalf("MCP function name = %q, want mcp__node_repl__js: %s", got, out)
	}
	if got := gjson.GetBytes(out, "tools.4.function.name").String(); got != "mcp__computer_use__list_apps" {
		t.Fatalf("computer use function name = %q, want mcp__computer_use__list_apps: %s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToChatCompletionsPreservesResponsesLiteToolHistory(t *testing.T) {
	request := `{
		"model":"gpt-test",
		"input":[
			{"type":"additional_tools","tools":[
				{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"Run code"}]},
				{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}
			]},
			{"type":"custom_tool_call","call_id":"call_exec","name":"exec","namespace":"functions","input":"text(true)"},
			{"type":"custom_tool_call_output","call_id":"call_exec","output":"ok"},
			{"type":"function_call","call_id":"call_spawn","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"inspect\"}"},
			{"type":"function_call_output","call_id":"call_spawn","output":"done"}
		]
	}`

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(request), false)
	if got := gjson.GetBytes(out, "messages.0.tool_calls.0.function.name").String(); got != "exec" {
		t.Fatalf("custom call name = %q, want exec: %s", got, out)
	}
	arguments := gjson.GetBytes(out, "messages.0.tool_calls.0.function.arguments").String()
	if got := gjson.Get(arguments, "input").String(); got != "text(true)" {
		t.Fatalf("custom call input = %q, want text(true): %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_exec" {
		t.Fatalf("custom output call id = %q, want call_exec: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.2.tool_calls.0.function.name").String(); got != "collaboration__spawn_agent" {
		t.Fatalf("function call name = %q, want collaboration__spawn_agent: %s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != "call_spawn" {
		t.Fatalf("function output call id = %q, want call_spawn: %s", got, out)
	}
}

func TestConvertChatCompletionsResponseRestoresResponsesLiteTools(t *testing.T) {
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteToolsRequest), false)
	chatResponse := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"text(true)\"}"}},{"id":"call_spawn","type":"function","function":{"name":"collaboration__spawn_agent","arguments":"{\"message\":\"inspect\"}"}},{"id":"call_js","type":"function","function":{"name":"mcp__node_repl__js","arguments":"{\"code\":\"1+1\"}"}}]}}]}`)

	out := ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(context.Background(), "gpt-test", []byte(responsesLiteToolsRequest), translated, chatResponse, nil)
	if got := gjson.Get(out, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("custom output type = %q, want custom_tool_call: %s", got, out)
	}
	if got := gjson.Get(out, "output.0.name").String(); got != "exec" {
		t.Fatalf("custom output name = %q, want exec: %s", got, out)
	}
	if got := gjson.Get(out, "output.0.input").String(); got != "text(true)" {
		t.Fatalf("custom output input = %q, want text(true): %s", got, out)
	}
	if got := gjson.Get(out, "output.1.namespace").String(); got != "collaboration" {
		t.Fatalf("function namespace = %q, want collaboration: %s", got, out)
	}
	if got := gjson.Get(out, "output.1.name").String(); got != "spawn_agent" {
		t.Fatalf("function name = %q, want spawn_agent: %s", got, out)
	}
	if got := gjson.Get(out, "output.2.namespace").String(); got != "mcp__node_repl__" {
		t.Fatalf("MCP namespace = %q, want mcp__node_repl__: %s", got, out)
	}
	if got := gjson.Get(out, "output.2.name").String(); got != "js" {
		t.Fatalf("MCP function name = %q, want js: %s", got, out)
	}
}

func TestConvertChatCompletionsStreamRestoresResponsesLiteTools(t *testing.T) {
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteToolsRequest), true)
	chunk := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"text(true)\"}"}},{"index":1,"id":"call_spawn","type":"function","function":{"name":"collaboration__spawn_agent","arguments":"{\"message\":\"inspect\"}"}}]},"finish_reason":"tool_calls"}]}`)
	var state any
	events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-test", []byte(responsesLiteToolsRequest), translated, chunk, &state)
	joined := strings.Join(events, "\n")

	if !strings.Contains(joined, `"type":"custom_tool_call"`) || !strings.Contains(joined, `"input":"text(true)"`) {
		t.Fatalf("stream did not restore custom tool call: %s", joined)
	}
	if !strings.Contains(joined, `"namespace":"collaboration"`) || !strings.Contains(joined, `"name":"spawn_agent"`) {
		t.Fatalf("stream did not restore namespaced function call: %s", joined)
	}
}

func TestConvertChatCompletionsStreamWaitsForResponsesLiteToolName(t *testing.T) {
	translated := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("gpt-test", []byte(responsesLiteToolsRequest), true)
	var state any
	idChunk := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_js","type":"function"}]}}]}`)
	events := ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-test", []byte(responsesLiteToolsRequest), translated, idChunk, &state)
	if joined := strings.Join(events, "\n"); strings.Contains(joined, "response.output_item.added") {
		t.Fatalf("tool item emitted before its name was known: %s", joined)
	}

	nameChunk := []byte(`{"id":"chatcmpl-1","created":1,"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"mcp__node_repl__js","arguments":"{\"code\":\"1+1\"}"}}]},"finish_reason":"tool_calls"}]}`)
	events = ConvertOpenAIChatCompletionsResponseToOpenAIResponses(context.Background(), "gpt-test", []byte(responsesLiteToolsRequest), translated, nameChunk, &state)
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"namespace":"mcp__node_repl__"`) || !strings.Contains(joined, `"name":"js"`) {
		t.Fatalf("split stream did not restore MCP namespace/name: %s", joined)
	}
}

package executor

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// xaiCodexNamespaceRequest is trimmed from what Codex CLI 0.166.0 sent for grok-4.5
// through a custom Responses provider (captured 2026-11-10 against a recording
// upstream, kittors/CliRelay#696). MCP servers arrive as namespaces named
// mcp__<server> without a trailing delimiter, two servers can expose the same
// child name, the built-in multi_agent_v1 namespace rides along on every
// request, and web_search carries external_web_access. The history replays an
// earlier namespaced call the way Codex stores it: name plus namespace.
const xaiCodexNamespaceRequest = `{
	"model":"grok-4.5",
	"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"search the docs"}]},
		{"type":"function_call","call_id":"call_prev","name":"search","namespace":"mcp__docs","arguments":"{\"query\":\"old\"}"},
		{"type":"function_call_output","call_id":"call_prev","output":"ok"}
	],
	"tools":[
		{"type":"function","name":"exec_command","description":"Run a command.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"namespace","name":"multi_agent_v1","description":"Tools for spawning and managing sub-agents.","tools":[
			{"type":"function","name":"spawn_agent","description":"Spawn a sub-agent.","strict":false,"parameters":{"type":"object","properties":{}}}
		]},
		{"type":"namespace","name":"mcp__demo","description":"Tools in the mcp__demo namespace.","tools":[
			{"type":"function","name":"fetch_page_with_a_rather_long_descriptive_tool_name_for_limits","description":"Fetch a page from demo.","strict":false,"parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}},
			{"type":"function","name":"search","description":"Search in demo.","strict":false,"parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
		]},
		{"type":"namespace","name":"mcp__docs","description":"Tools in the mcp__docs namespace.","tools":[
			{"type":"function","name":"fetch_page_with_a_rather_long_descriptive_tool_name_for_limits","description":"Fetch a page from docs.","strict":false,"parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}},
			{"type":"function","name":"search","description":"Search in docs.","strict":false,"parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
		]},
		{"type":"web_search","external_web_access":false}
	],
	"tool_choice":"auto",
	"parallel_tool_calls":true,
	"stream":true,
	"store":false,
	"include":["reasoning.encrypted_content"],
	"prompt_cache_key":"019f7b1c-0000-7000-8000-000000000696"
}`

// xaiNamespaceUpstreamEvents is what Grok answers with once it only sees the
// flattened names: two namespaced calls and one call to a top-level tool that
// must come back untouched.
var xaiNamespaceUpstreamEvents = []string{
	`data: {"type":"response.created","response":{"id":"resp_ns","model":"grok-4.5","status":"in_progress","output":[]}}`,
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp__demo__search","arguments":"","status":"in_progress"}}`,
	`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"query\":\"grok\"}"}`,
	`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","name":"mcp__demo__search","arguments":"{\"query\":\"grok\"}"}`,
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp__demo__search","arguments":"{\"query\":\"grok\"}","status":"completed"}}`,
	`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"multi_agent_v1__spawn_agent","arguments":"{}","status":"completed"}}`,
	`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_3","call_id":"call_3","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","status":"completed"}}`,
	`data: {"type":"response.completed","response":{"id":"resp_ns","model":"grok-4.5","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp__demo__search","arguments":"{\"query\":\"grok\"}","status":"completed"},` +
		`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"multi_agent_v1__spawn_agent","arguments":"{}","status":"completed"},` +
		`{"type":"function_call","id":"fc_3","call_id":"call_3","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","status":"completed"}` +
		`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
}

// xaiWireToolNamePattern is the function-name shape OpenAI-compatible
// upstreams accept; flattened names must stay inside it.
var xaiWireToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func xaiNamespaceTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"auth_kind": "oauth",
			"api_key":   "oauth-token",
			"base_url":  xaiauth.DefaultAPIBaseURL,
			"using_api": "false",
		},
	}
}

// xaiRecordingUpstream records every request body the executor sends and
// answers each one with the given SSE events, standing in for Grok Build.
func xaiRecordingUpstream(t *testing.T, bodies *[][]byte, events []string) context.Context {
	t.Helper()
	return context.WithValue(context.Background(), util.ContextKeyRoundTripper, xaiStreamRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read upstream request: %v", err)
		}
		*bodies = append(*bodies, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(strings.Join(events, "\n\n") + "\n\n")),
			Request:    req,
		}, nil
	}))
}

func xaiCollectStream(t *testing.T, result *cliproxyexecutor.StreamResult) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		for _, line := range strings.Split(string(chunk.Payload), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			events = append(events, gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		}
	}
	return events
}

func assertXAIToolCall(t *testing.T, where string, item gjson.Result, wantName, wantNamespace string) {
	t.Helper()
	if got := item.Get("name").String(); got != wantName {
		t.Fatalf("%s: name = %q, want %q: %s", where, got, wantName, item.Raw)
	}
	namespace := item.Get("namespace")
	if wantNamespace == "" {
		if namespace.Exists() {
			t.Fatalf("%s: unexpected namespace %q on a top-level call: %s", where, namespace.String(), item.Raw)
		}
		return
	}
	if namespace.String() != wantNamespace {
		t.Fatalf("%s: namespace = %q, want %q: %s", where, namespace.String(), wantNamespace, item.Raw)
	}
}

// assertXAIOutboundNamespaceFlattened checks the body Grok received for the
// Codex namespace request: no namespace container left, every child reachable
// under a distinct valid name, and the history call renamed to match.
func assertXAIOutboundNamespaceFlattened(t *testing.T, body []byte) {
	t.Helper()
	names := map[string]gjson.Result{}
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		switch tool.Get("type").String() {
		case "namespace":
			t.Fatalf("namespace tool reached xAI: %s", tool.Raw)
		case "function":
			name := tool.Get("name").String()
			if !xaiWireToolNamePattern.MatchString(name) {
				t.Fatalf("function name %q is not a valid upstream name", name)
			}
			if _, dup := names[name]; dup {
				t.Fatalf("function name %q sent twice: %s", name, body)
			}
			names[name] = tool
		case "web_search":
			if tool.Get("external_web_access").Exists() {
				t.Fatalf("web_search kept external_web_access, which xAI rejects: %s", tool.Raw)
			}
		}
	}
	for _, want := range []string{"exec_command", "multi_agent_v1__spawn_agent", "mcp__demo__search", "mcp__docs__search"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("flattened tools missing %q: %s", want, gjson.GetBytes(body, "tools").Raw)
		}
	}
	if got := names["mcp__docs__search"].Get("description").String(); got != "Search in docs." {
		t.Fatalf("mcp__docs__search carries the wrong definition: %q", got)
	}
	var longNames []string
	for name, tool := range names {
		if strings.HasPrefix(tool.Get("description").String(), "Fetch a page") {
			longNames = append(longNames, name)
		}
	}
	if len(longNames) != 2 || longNames[0] == longNames[1] {
		t.Fatalf("long child names must stay distinct after shortening, got %v", longNames)
	}
	if got := gjson.GetBytes(body, `input.#(call_id=="call_prev")`); got.Get("type").String() != "function_call" {
		t.Fatalf("history call missing from outbound input: %s", gjson.GetBytes(body, "input").Raw)
	} else {
		assertXAIToolCall(t, "history call", got, "mcp__docs__search", "")
	}
	if got := gjson.GetBytes(body, "tool_choice").String(); got != "auto" {
		t.Fatalf("tool_choice = %q, want auto", got)
	}
}

func TestXAIExecutorStreamFlattensCodexNamespaceTools(t *testing.T) {
	var bodies [][]byte
	ctx := xaiRecordingUpstream(t, &bodies, xaiNamespaceUpstreamEvents)
	payload := []byte(xaiCodexNamespaceRequest)
	result, err := NewXAIExecutor(&config.Config{}).ExecuteStream(ctx, xaiNamespaceTestAuth(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	events := xaiCollectStream(t, result)
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	assertXAIOutboundNamespaceFlattened(t, bodies[0])

	seen := map[string]int{}
	for _, event := range events {
		switch eventType := event.Get("type").String(); eventType {
		case "response.output_item.added", "response.output_item.done":
			item := event.Get("item")
			switch item.Get("id").String() {
			case "fc_1":
				assertXAIToolCall(t, eventType, item, "search", "mcp__demo")
			case "fc_2":
				assertXAIToolCall(t, eventType, item, "spawn_agent", "multi_agent_v1")
			case "fc_3":
				assertXAIToolCall(t, eventType, item, "exec_command", "")
			}
			seen[eventType+"/"+item.Get("id").String()]++
		case "response.function_call_arguments.done":
			assertXAIToolCall(t, eventType, event, "search", "mcp__demo")
			seen[eventType]++
		case "response.completed":
			output := event.Get("response.output").Array()
			if len(output) != 3 {
				t.Fatalf("completed output has %d items, want 3: %s", len(output), event.Raw)
			}
			assertXAIToolCall(t, "completed output 0", output[0], "search", "mcp__demo")
			assertXAIToolCall(t, "completed output 1", output[1], "spawn_agent", "multi_agent_v1")
			assertXAIToolCall(t, "completed output 2", output[2], "exec_command", "")
			seen[eventType]++
		}
	}
	for _, key := range []string{
		"response.output_item.added/fc_1",
		"response.output_item.done/fc_1",
		"response.output_item.done/fc_2",
		"response.output_item.done/fc_3",
		"response.function_call_arguments.done",
		"response.completed",
	} {
		if seen[key] != 1 {
			t.Fatalf("event %s seen %d times, want 1 (seen=%v)", key, seen[key], seen)
		}
	}
}

func TestXAIExecutorNonStreamFlattensCodexNamespaceTools(t *testing.T) {
	var bodies [][]byte
	ctx := xaiRecordingUpstream(t, &bodies, xaiNamespaceUpstreamEvents)
	payload := []byte(strings.Replace(xaiCodexNamespaceRequest, `"stream":true`, `"stream":false`, 1))
	resp, err := NewXAIExecutor(&config.Config{}).Execute(ctx, xaiNamespaceTestAuth(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	assertXAIOutboundNamespaceFlattened(t, bodies[0])

	output := gjson.GetBytes(resp.Payload, "output").Array()
	if len(output) != 3 {
		t.Fatalf("response output has %d items, want 3: %s", len(output), resp.Payload)
	}
	assertXAIToolCall(t, "output 0", output[0], "search", "mcp__demo")
	assertXAIToolCall(t, "output 1", output[1], "spawn_agent", "multi_agent_v1")
	assertXAIToolCall(t, "output 2", output[2], "exec_command", "")
}

// Codex's Responses Lite shape carries the tool list in an additional_tools
// input item instead of top-level tools; xAI knows neither that item nor
// namespaces, so both have to be lifted into plain top-level tools.
func TestXAIExecutorFlattensResponsesLiteAdditionalTools(t *testing.T) {
	liteRequest := []byte(`{
		"model":"grok-4.5",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"exec_command","description":"Run a command.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
				{"type":"namespace","name":"mcp__demo","description":"Tools in the mcp__demo namespace.","tools":[
					{"type":"function","name":"search","description":"Search in demo.","strict":false,"parameters":{"type":"object","properties":{"query":{"type":"string"}}}}
				]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"search the demo"}]}
		],
		"tools":[],
		"tool_choice":"auto",
		"stream":true
	}`)
	var bodies [][]byte
	ctx := xaiRecordingUpstream(t, &bodies, xaiNamespaceUpstreamEvents)
	result, err := NewXAIExecutor(&config.Config{}).ExecuteStream(ctx, xaiNamespaceTestAuth(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: liteRequest,
	}, cliproxyexecutor.Options{
		Stream:          true,
		OriginalRequest: liteRequest,
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	events := xaiCollectStream(t, result)
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	body := bodies[0]
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			t.Fatalf("additional_tools item reached xAI: %s", body)
		}
	}
	if got := gjson.GetBytes(body, "input.0.type").String(); got != "message" {
		t.Fatalf("user message lost while lifting additional_tools: %s", gjson.GetBytes(body, "input").Raw)
	}
	var names []string
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() == "namespace" {
			t.Fatalf("namespace tool reached xAI: %s", tool.Raw)
		}
		names = append(names, tool.Get("name").String())
	}
	if strings.Join(names, ",") != "exec_command,mcp__demo__search" {
		t.Fatalf("lifted tools = %v, want [exec_command mcp__demo__search]", names)
	}
	restored := false
	for _, event := range events {
		if event.Get("type").String() == "response.output_item.done" && event.Get("item.id").String() == "fc_1" {
			assertXAIToolCall(t, "lite output_item.done", event.Get("item"), "search", "mcp__demo")
			restored = true
		}
	}
	if !restored {
		t.Fatalf("namespaced call never reached the client")
	}
}

// A tool_choice that forces one namespaced child must name the flattened tool:
// xAI knows neither the namespace field nor the bare child name.
func TestXAIExecutorQualifiesForcedNamespaceToolChoice(t *testing.T) {
	payload := []byte(strings.Replace(xaiCodexNamespaceRequest, `"tool_choice":"auto"`, `"tool_choice":{"type":"function","name":"search","namespace":"mcp__docs"}`, 1))
	var bodies [][]byte
	ctx := xaiRecordingUpstream(t, &bodies, xaiNamespaceUpstreamEvents)
	if _, err := NewXAIExecutor(&config.Config{}).Execute(ctx, xaiNamespaceTestAuth(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FormatOpenAIResponse,
	}); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	choice := gjson.GetBytes(bodies[0], "tool_choice")
	if choice.Get("type").String() != "function" || choice.Get("name").String() != "mcp__docs__search" || choice.Get("namespace").Exists() {
		t.Fatalf("tool_choice = %s, want {\"type\":\"function\",\"name\":\"mcp__docs__search\"}", choice.Raw)
	}
}

// Requests without namespaces must reach xAI exactly as before, including a
// Claude Code tool whose parameters happen to have a property named namespace.
func TestFlattenXAINamespaceToolsLeavesPlainRequestsUntouched(t *testing.T) {
	for name, body := range map[string]string{
		"no tools": `{"model":"grok-4.5","input":"hi"}`,
		"tool parameter named namespace": `{"model":"grok-4.5","input":[{"type":"function_call","call_id":"c1","name":"kubectl_get","arguments":"{\"namespace\":\"default\"}"}],` +
			`"tools":[{"type":"function","name":"kubectl_get","parameters":{"type":"object","properties":{"namespace":{"type":"string"}}}}],` +
			`"tool_choice":{"type":"function","name":"kubectl_get"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, names := flattenXAINamespaceTools([]byte(body))
			if names != nil {
				t.Fatalf("plain request produced a name mapping: %v", names.byWire)
			}
			if string(out) != body {
				t.Fatalf("plain request changed:\n got %s\nwant %s", out, body)
			}
		})
	}
}

func TestXAIPrepareResponsesRequestKeepsClaudeToolsUnmapped(t *testing.T) {
	payload := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"list pods"}],
		"tools":[{"name":"kubectl_get","description":"Get resources","input_schema":{"type":"object","properties":{"namespace":{"type":"string"}}}}]
	}`)
	_, body, toolNames, err := NewXAIExecutor(&config.Config{}).prepareResponsesRequest(
		context.Background(),
		&cliproxyauth.Auth{Provider: "xai"},
		cliproxyexecutor.Request{Model: "grok-4.5", Payload: payload},
		cliproxyexecutor.Options{OriginalRequest: payload, SourceFormat: sdktranslator.FormatClaude},
		false,
	)
	if err != nil {
		t.Fatalf("prepareResponsesRequest returned error: %v", err)
	}
	if toolNames != nil {
		t.Fatalf("Claude request produced a name mapping: %v", toolNames.byWire)
	}
	if got := gjson.GetBytes(body, "tools.0.name").String(); got != "kubectl_get" {
		t.Fatalf("Claude tool renamed to %q: %s", got, body)
	}
	if !gjson.GetBytes(body, "tools.0.parameters.properties.namespace").Exists() {
		t.Fatalf("Claude tool lost its namespace parameter: %s", body)
	}
}

// Qualified names can collide with a top-level tool, with a child that is
// already qualified, and across namespaces whose children start with mcp__.
// Every one of them still needs its own name and its own way back.
func TestFlattenXAINamespaceToolsAvoidsNameCollisions(t *testing.T) {
	body := []byte(`{"tools":[
		{"type":"function","name":"mcp__demo__search","description":"top-level"},
		{"type":"namespace","name":"mcp__demo","tools":[
			{"type":"function","name":"search","description":"demo child"},
			{"type":"function","name":"mcp__demo__search","description":"already qualified child"}
		]},
		{"type":"namespace","name":"a","tools":[{"type":"function","name":"mcp__x","description":"a child"}]},
		{"type":"namespace","name":"b","tools":[{"type":"function","name":"mcp__x","description":"b child"}]},
		{"type":"namespace","name":"my.server","tools":[{"type":"function","name":"do it","description":"unsanitized child"}]}
	]}`)
	out, names := flattenXAINamespaceTools(body)
	wireByDescription := map[string]string{}
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		name := tool.Get("name").String()
		if !xaiWireToolNamePattern.MatchString(name) {
			t.Fatalf("invalid upstream name %q", name)
		}
		for description, other := range wireByDescription {
			if other == name {
				t.Fatalf("%q and %q both sent as %q", description, tool.Get("description").String(), name)
			}
		}
		wireByDescription[tool.Get("description").String()] = name
	}
	if len(wireByDescription) != 6 {
		t.Fatalf("flattened tools = %v, want 6 distinct entries", wireByDescription)
	}
	if got := wireByDescription["top-level"]; got != "mcp__demo__search" {
		t.Fatalf("top-level tool renamed to %q", got)
	}
	if wireByDescription["a child"] != "a__mcp__x" || wireByDescription["b child"] != "b__mcp__x" {
		t.Fatalf("mcp__ children must keep their namespace prefix: %v", wireByDescription)
	}
	if got := wireByDescription["unsanitized child"]; got != "my_server__do_it" {
		t.Fatalf("unsanitized child sent as %q, want my_server__do_it", got)
	}
	for description, want := range map[string]xaiNamespacedTool{
		"demo child":              {namespace: "mcp__demo", name: "search"},
		"already qualified child": {namespace: "mcp__demo", name: "mcp__demo__search"},
		"a child":                 {namespace: "a", name: "mcp__x"},
		"b child":                 {namespace: "b", name: "mcp__x"},
		"unsanitized child":       {namespace: "my.server", name: "do it"},
	} {
		if got := names.byWire[wireByDescription[description]]; got != want {
			t.Fatalf("%s maps back to %+v, want %+v", description, got, want)
		}
	}
	if _, ok := names.byWire["mcp__demo__search"]; ok {
		t.Fatalf("calls to the top-level tool would be rewritten into the namespace")
	}
}

func TestXAINamespaceWireNamesFitTheUpstreamLimit(t *testing.T) {
	namespace := "mcp__" + strings.Repeat("very_long_server_name_", 4)
	child := strings.Repeat("an_equally_long_tool_name_", 4)
	first := newXAINamespaceToolNames(map[string]struct{}{}).wireName(namespace, child)
	if len(first) > xaiMaxToolNameLength || !xaiWireToolNamePattern.MatchString(first) {
		t.Fatalf("wire name %q (%d bytes) does not fit the upstream limit", first, len(first))
	}
	if !strings.HasPrefix(first, "mcp__very_long_server_name_") {
		t.Fatalf("shortened name %q lost its readable prefix", first)
	}
	// Every turn re-sends the history, so the same tool must get the same name.
	if again := newXAINamespaceToolNames(map[string]struct{}{}).wireName(namespace, child); again != first {
		t.Fatalf("wire name not deterministic: %q then %q", first, again)
	}
	if other := newXAINamespaceToolNames(map[string]struct{}{}).wireName(namespace, child+"2"); other == first {
		t.Fatalf("two long names sharing a prefix both became %q", first)
	}
}

// Codex treats functions::exec and a plain exec as one tool, so the default
// namespace is unwrapped without a prefix or a mapping. This is the code-mode
// shape Codex sends as Responses Lite.
func TestFlattenXAINamespaceToolsUnwrapsTheDefaultNamespace(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec","description":"code mode"}]}]},
		{"type":"custom_tool_call","call_id":"c1","name":"exec","namespace":"functions","input":"print(1)"},
		{"type":"custom_tool_call_output","call_id":"c1","output":"1"}
	],"tools":[]}`)
	out, names := flattenXAINamespaceTools(body)
	if names != nil {
		t.Fatalf("default-namespace children need no mapping, got %v", names.byWire)
	}
	tools := gjson.GetBytes(out, "tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "custom" || tools[0].Get("name").String() != "exec" {
		t.Fatalf("tools = %s, want the unwrapped custom exec tool", gjson.GetBytes(out, "tools").Raw)
	}
	input := gjson.GetBytes(out, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input = %s, want the additional_tools item removed", gjson.GetBytes(out, "input").Raw)
	}
	assertXAIToolCall(t, "history call", input[0], "exec", "")
}

func TestFlattenXAINamespaceToolsRewritesAllowedTools(t *testing.T) {
	body := []byte(`{"tools":[
		{"type":"function","name":"exec_command"},
		{"type":"namespace","name":"mcp__demo","tools":[{"type":"function","name":"search"},{"type":"function","name":"fetch"}]},
		{"type":"namespace","name":"mcp__docs","tools":[{"type":"function","name":"search"}]}
	],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[
		{"type":"function","name":"exec_command"},
		{"type":"function","name":"search","namespace":"mcp__docs"},
		{"type":"namespace","name":"mcp__demo"}
	]}}`)
	out, _ := flattenXAINamespaceTools(body)
	var got []string
	for _, entry := range gjson.GetBytes(out, "tool_choice.tools").Array() {
		if entry.Get("namespace").Exists() || entry.Get("type").String() == xaiNamespaceToolType {
			t.Fatalf("allowed_tools entry still namespaced: %s", entry.Raw)
		}
		got = append(got, entry.Get("name").String())
	}
	if strings.Join(got, ",") != "exec_command,mcp__docs__search,mcp__demo__search,mcp__demo__fetch" {
		t.Fatalf("allowed_tools = %v", got)
	}
	if gjson.GetBytes(out, "tool_choice.mode").String() != "auto" {
		t.Fatalf("allowed_tools lost its mode: %s", gjson.GetBytes(out, "tool_choice").Raw)
	}
}

// A namespace the client stopped declaring still has calls in the history;
// they cannot carry a namespace field to xAI either.
func TestFlattenXAINamespaceToolsRenamesHistoryForUndeclaredNamespaces(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"c1","name":"search","namespace":"mcp__gone","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}],"tools":[{"type":"function","name":"exec_command"}]}`)
	out, _ := flattenXAINamespaceTools(body)
	assertXAIToolCall(t, "history call", gjson.GetBytes(out, "input.0"), "mcp__gone__search", "")
	if got := gjson.GetBytes(out, "tools").Raw; got != `[{"type":"function","name":"exec_command"}]` {
		t.Fatalf("tools changed without a namespace to flatten: %s", got)
	}
}

func TestFlattenXAINamespaceToolsDeduplicatesLiftedLiteTools(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"function","name":"exec_command","description":"lite copy"},
			{"type":"web_search"},
			{"type":"namespace","name":"mcp__demo","tools":[{"type":"function","name":"search","description":"lite copy"}]}
		]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	],"tools":[
		{"type":"function","name":"exec_command","description":"top-level"},
		{"type":"web_search"},
		{"type":"namespace","name":"mcp__demo","tools":[{"type":"function","name":"search","description":"top-level"}]}
	]}`)
	out, _ := flattenXAINamespaceTools(body)
	var got []string
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		got = append(got, tool.Get("type").String()+":"+tool.Get("name").String()+":"+tool.Get("description").String())
	}
	if want := "function:exec_command:top-level,web_search::,function:mcp__demo__search:top-level"; strings.Join(got, ",") != want {
		t.Fatalf("tools = %v, want %s", got, want)
	}
}

func TestXAINamespaceCustomToolRoundTrip(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"mcp__patch","tools":[{"type":"custom","name":"apply","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]}]}`)
	out, names := flattenXAINamespaceTools(body)
	tool := gjson.GetBytes(out, "tools.0")
	if tool.Get("type").String() != "custom" || tool.Get("name").String() != "mcp__patch__apply" || !tool.Get("format").Exists() {
		t.Fatalf("custom child = %s, want custom mcp__patch__apply with its format", tool.Raw)
	}
	event := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"c1","name":"mcp__patch__apply","input":"*** Begin Patch"}}`)
	restored, changed := names.restoreEvent(event)
	if !changed {
		t.Fatalf("custom call was not restored: %s", restored)
	}
	assertXAIToolCall(t, "custom call", gjson.GetBytes(restored, "item"), "apply", "mcp__patch")
	if got := gjson.GetBytes(restored, "item.input").String(); got != "*** Begin Patch" {
		t.Fatalf("custom call input = %q", got)
	}
}

func TestXAINamespaceRestoreLeavesOtherEventsAlone(t *testing.T) {
	_, names := flattenXAINamespaceTools([]byte(xaiCodexNamespaceRequest))
	if names.empty() {
		t.Fatalf("Codex request produced no name mapping")
	}
	for _, raw := range []string{
		`{"type":"response.output_text.delta","delta":"mcp__demo__search"}`,
		`{"type":"response.output_item.done","item":{"type":"message","name":"mcp__demo__search"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","name":"exec_command"}}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{}"}`,
	} {
		if out, changed := names.restoreEvent([]byte(raw)); changed || string(out) != raw {
			t.Fatalf("event rewritten: %s -> %s", raw, out)
		}
	}
	if line := []byte("event: response.output_item.done"); string(names.restoreSSELine(line)) != string(line) {
		t.Fatalf("non-data line rewritten")
	}
	var noNames *xaiNamespaceToolNames
	line := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"mcp__demo__search"}}`)
	if got := noNames.restoreSSELine(line); string(got) != string(line) {
		t.Fatalf("request without namespaces rewrote a call: %s", got)
	}
}

func TestStripXAIUnsupportedWebSearchOptions(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"external_web_access_probe","parameters":{"type":"object"}},{"type":"web_search","external_web_access":false,"search_context_size":"high","filters":{"allowed_domains":["x.ai"]}}]}`)
	out := stripXAIUnsupportedWebSearchOptions(body)
	if got := gjson.GetBytes(out, "tools.1").Raw; got != `{"type":"web_search","filters":{"allowed_domains":["x.ai"]}}` {
		t.Fatalf("web_search = %s", got)
	}
	if got := gjson.GetBytes(out, "tools.0").Raw; got != `{"type":"function","name":"external_web_access_probe","parameters":{"type":"object"}}` {
		t.Fatalf("function tool changed: %s", got)
	}
	plain := []byte(`{"tools":[{"type":"web_search"}]}`)
	if got := stripXAIUnsupportedWebSearchOptions(plain); string(got) != string(plain) {
		t.Fatalf("plain web_search changed: %s", got)
	}
}

// Codex replays every earlier call on the next turn. A call to a child whose
// name had to be shortened must come out under the same name its declaration
// got, or Grok sees a call to a tool that does not exist.
func TestFlattenXAINamespaceToolsKeepsHistoryNamesInStepWithTools(t *testing.T) {
	body := strings.Replace(xaiCodexNamespaceRequest,
		`"name":"search","namespace":"mcp__docs"`,
		`"name":"fetch_page_with_a_rather_long_descriptive_tool_name_for_limits","namespace":"mcp__docs"`, 1)
	out, _ := flattenXAINamespaceTools([]byte(body))
	declared := gjson.GetBytes(out, `tools.#(description=="Fetch a page from docs.").name`).String()
	if declared == "" || len(declared) > xaiMaxToolNameLength {
		t.Fatalf("long docs child declared as %q", declared)
	}
	assertXAIToolCall(t, "history call", gjson.GetBytes(out, `input.#(call_id=="call_prev")`), declared, "")
}

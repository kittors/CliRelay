package executor

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// xaiZodV4UnionParameters is what Codex CLI 0.166.0 sent for an MCP tool whose
// inputSchema came from zod v4's toJSONSchema (captured 2026-11-10 against a
// recording upstream): an object root whose oneOf branches are $refs, one of
// them into a single-branch oneOf. Codex only turned const into enum. Grok
// rejects the request with "root schema is an anyOf/oneOf union with a
// non-object branch".
const xaiZodV4UnionParameters = `{"type":"object","properties":{},"oneOf":[{"$ref":"#/$defs/__schema0"},{"$ref":"#/$defs/__schema1"}],` +
	`"$defs":{"__schema0":{"type":"object","properties":{"id":{"type":"string"},"mode":{"type":"string","enum":["view"]}},"required":["mode","id"],"additionalProperties":false},` +
	`"__schema1":{"oneOf":[{"type":"object","properties":{"mode":{"type":"string","enum":["create"]},"name":{"type":"string"}},"required":["mode","name"],"additionalProperties":false}]}}}`

const (
	xaiZodV4ViewBranch   = `{"type":"object","properties":{"id":{"type":"string"},"mode":{"type":"string","enum":["view"]}},"required":["mode","id"],"additionalProperties":false}`
	xaiZodV4CreateBranch = `{"type":"object","properties":{"mode":{"type":"string","enum":["create"]},"name":{"type":"string"}},"required":["mode","name"],"additionalProperties":false}`
)

func xaiSchemaToolBody(parameters string) []byte {
	return []byte(`{"model":"grok-4.7","input":"hi","tools":[{"type":"function","name":"probe","strict":false,"parameters":` + parameters + `}]}`)
}

func assertXAIJSONEqual(t *testing.T, where, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("%s: got invalid JSON %v: %s", where, err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("%s: want invalid JSON %v: %s", where, err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s:\n got %s\nwant %s", where, got, want)
	}
}

// assertXAIRootUnionAccepted fails unless every root union branch says
// "type":"object" itself, the only kind Grok accepts.
func assertXAIRootUnionAccepted(t *testing.T, where string, parameters gjson.Result) {
	t.Helper()
	if !parameters.Get("type").Exists() && !parameters.Get("anyOf").Exists() && !parameters.Get("oneOf").Exists() {
		t.Fatalf("%s: parameters root has neither a type nor a union: %s", where, parameters.Raw)
	}
	for _, key := range xaiSchemaUnionKeys {
		for _, branch := range parameters.Get(key).Array() {
			if branch.Get("type").String() != "object" {
				t.Fatalf("%s: %s branch is not an explicit object: %s", where, key, branch.Raw)
			}
		}
	}
}

func TestNormalizeXAIToolParameterRootsRewritesRejectedRoots(t *testing.T) {
	for name, tc := range map[string]struct {
		parameters string
		want       string
	}{
		"zod v4 oneOf of refs": {
			parameters: xaiZodV4UnionParameters,
			want: `{"type":"object","properties":{},"oneOf":[` + xaiZodV4ViewBranch + `,` + xaiZodV4CreateBranch + `],` +
				`"$defs":` + gjson.Get(xaiZodV4UnionParameters, "$defs").Raw + `}`,
		},
		"untyped object branches": {
			parameters: `{"anyOf":[{"properties":{"a":{"type":"string"}},"required":["a"]},{"properties":{"b":{"type":"number"}}}]}`,
			want:       `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"number"}}}]}`,
		},
		"branches that are never an object": {
			parameters: `{"type":"object","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"null"},{"type":["string","integer"]},false]}`,
			want:       `{"type":"object","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}}]}`,
		},
		"nullable object branch": {
			parameters: `{"oneOf":[{"type":["object","null"],"properties":{"a":{"type":"string"}}},{"type":"object"}]}`,
			want:       `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object"}]}`,
		},
		"permissive and unresolvable branches": {
			parameters: `{"type":"object","oneOf":[true,{},{"$ref":"https://example.com/schema.json"},{"$ref":"#/$defs/missing"}],"$defs":{}}`,
			want:       `{"type":"object","oneOf":[{"type":"object"},{"type":"object"},{"type":"object"},{"type":"object"}],"$defs":{}}`,
		},
		"reference cycle": {
			parameters: `{"type":"object","oneOf":[{"$ref":"#/$defs/loop"}],"$defs":{"loop":{"$ref":"#/$defs/loop"}}}`,
			want:       `{"type":"object","oneOf":[{"type":"object"}],"$defs":{"loop":{"$ref":"#/$defs/loop"}}}`,
		},
		"pointer escapes and definitions": {
			parameters: `{"type":"object","anyOf":[{"$ref":"#/definitions/a~1b"},{"$ref":"#/definitions/c~0d"}],"definitions":{"a/b":{"type":"object","properties":{"x":{"type":"string"}}},"c~d":{"properties":{"y":{"type":"string"}}}}}`,
			want: `{"type":"object","anyOf":[{"type":"object","properties":{"x":{"type":"string"}}},{"type":"object","properties":{"y":{"type":"string"}}}],` +
				`"definitions":{"a/b":{"type":"object","properties":{"x":{"type":"string"}}},"c~d":{"properties":{"y":{"type":"string"}}}}}`,
		},
		"nested union with annotations flattens": {
			parameters: `{"oneOf":[{"description":"either form","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"string"}]},{"type":"object"}]}`,
			want:       `{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object"}]}`,
		},
		"nested union with constraints stays a typed branch": {
			parameters: `{"oneOf":[{"properties":{"kind":{"type":"string"}},"anyOf":[{"required":["a"]},{"required":["b"]}]},{"type":"object"}]}`,
			want:       `{"oneOf":[{"type":"object","properties":{"kind":{"type":"string"}},"anyOf":[{"required":["a"]},{"required":["b"]}]},{"type":"object"}]}`,
		},
		"every branch dropped": {
			parameters: `{"oneOf":[{"type":"string"},{"type":"number"}],"description":"a value"}`,
			want:       `{"description":"a value","type":"object"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := normalizeXAIToolParameterRoots(xaiSchemaToolBody(tc.parameters))
			parameters := gjson.GetBytes(out, "tools.0.parameters")
			assertXAIJSONEqual(t, "parameters", parameters.Raw, tc.want)
			assertXAIRootUnionAccepted(t, "parameters", parameters)
			if got := gjson.GetBytes(out, "tools.0.name").String(); got != "probe" {
				t.Fatalf("tool renamed to %q", got)
			}
		})
	}
}

// Shapes Grok accepted on 2026-11-10 must reach it unchanged, down to the byte.
func TestNormalizeXAIToolParameterRootsKeepsAcceptedRootsByteForByte(t *testing.T) {
	for name, body := range map[string]string{
		"no union": string(xaiSchemaToolBody(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}`)),
		// Codex Desktop 26.1103 automation_update, trimmed: unions and type
		// arrays inside properties are fine.
		"unions inside properties": string(xaiSchemaToolBody(`{"type":"object","properties":{"cwds":{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},` +
			`"localEnvironmentConfigPath":{"type":["string","null"]}},"additionalProperties":false}`)),
		"object-only root oneOf": string(xaiSchemaToolBody(`{"type":"object","properties":{},"oneOf":[` + xaiZodV4ViewBranch + `,` + xaiZodV4CreateBranch + `]}`)),
		// zod v3 after Codex typed its branches.
		"untyped root with typed branches": string(xaiSchemaToolBody(`{"properties":{},"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"number"}}}]}`)),
		"ref inside properties":            string(xaiSchemaToolBody(`{"type":"object","properties":{"item":{"$ref":"#/$defs/Item"}},"$defs":{"Item":{"type":"object","properties":{"title":{"type":"string"}}}}}`)),
		"non-function tools": `{"model":"grok-4.7","tools":[{"type":"custom","name":"apply_patch","format":{"type":"grammar","definition":"oneOf"}},` +
			`{"type":"web_search","filters":{"anyOf":[]}}]}`,
		"no tools": `{"model":"grok-4.7","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what does oneOf mean"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if out := normalizeXAIToolParameterRoots([]byte(body)); string(out) != body {
				t.Fatalf("accepted request changed:\n got %s\nwant %s", out, body)
			}
		})
	}
}

// xaiCodexUnionToolsRequest carries both ways a rejected root reaches CliRelay
// from Codex: an MCP tool built with zod v4, and a Desktop dynamic tool (the
// codex_app.automation_update shape upstream CLIProxyAPI reports from older
// Desktop builds; 26.1103 sends a plain object root).
const xaiCodexUnionToolsRequest = `{
	"model":"grok-4.7",
	"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"show automation a1"}]}],
	"tools":[
		{"type":"function","name":"exec_command","description":"Run a command.","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
		{"type":"namespace","name":"mcp__forms","description":"Tools in the mcp__forms namespace.","tools":[
			{"type":"function","name":"submit","description":"Submit a form.","strict":false,"parameters":` + xaiZodV4UnionParameters + `}
		]},
		{"type":"namespace","name":"codex_app","description":"Tools provided by the Codex app.","tools":[
			{"type":"function","name":"automation_update","description":"Manage automations.","strict":false,"parameters":` + xaiZodV4UnionParameters + `},
			{"type":"function","name":"read_thread_terminal","description":"Read the terminal.","strict":false,"parameters":{"type":"object","properties":{},"additionalProperties":false}}
		]},
		{"type":"web_search","external_web_access":false}
	],
	"tool_choice":"auto",
	"parallel_tool_calls":true,
	"stream":true,
	"store":false
}`

var xaiUnionToolsUpstreamEvents = []string{
	`data: {"type":"response.created","response":{"id":"resp_union","model":"grok-4.7","status":"in_progress","output":[]}}`,
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"codex_app__automation_update","arguments":"{\"mode\":\"view\",\"id\":\"a1\"}","status":"completed"}}`,
	`data: {"type":"response.completed","response":{"id":"resp_union","model":"grok-4.7","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"codex_app__automation_update","arguments":"{\"mode\":\"view\",\"id\":\"a1\"}","status":"completed"}` +
		`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
}

func TestXAIExecutorSendsObjectBranchesForCodexRootUnions(t *testing.T) {
	var bodies [][]byte
	ctx := xaiRecordingUpstream(t, &bodies, xaiUnionToolsUpstreamEvents)
	payload := []byte(xaiCodexUnionToolsRequest)
	result, err := NewXAIExecutor(&config.Config{}).ExecuteStream(ctx, xaiNamespaceTestAuth(), cliproxyexecutor.Request{
		Model:   "grok-4.7",
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

	tools := map[string]gjson.Result{}
	for _, tool := range gjson.GetBytes(bodies[0], "tools").Array() {
		tools[tool.Get("name").String()] = tool
	}
	for _, name := range []string{"mcp__forms__submit", "codex_app__automation_update"} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("flattened tool %q missing upstream: %s", name, gjson.GetBytes(bodies[0], "tools").Raw)
		}
		parameters := tool.Get("parameters")
		assertXAIRootUnionAccepted(t, name, parameters)
		if branches := parameters.Get("oneOf").Array(); len(branches) != 2 || branches[0].Get("properties.mode.enum.0").String() != "view" || branches[1].Get("properties.mode.enum.0").String() != "create" {
			t.Fatalf("%s lost its alternatives: %s", name, parameters.Raw)
		}
	}
	if got := tools["exec_command"].Get("parameters").Raw; got != `{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}` {
		t.Fatalf("exec_command parameters changed: %s", got)
	}

	sawCall := false
	for _, event := range events {
		if event.Get("type").String() == "response.output_item.done" {
			assertXAIToolCall(t, "output_item.done", event.Get("item"), "automation_update", "codex_app")
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("the automation_update call never reached the client")
	}
}

// Claude Code's MCP tools travel as input_schema and hit the same check once
// translated, so the rewrite is not tied to Codex.
func TestXAIPrepareResponsesRequestRewritesClaudeToolRootUnion(t *testing.T) {
	payload := []byte(`{
		"model":"grok-4.7",
		"messages":[{"role":"user","content":"submit the form"}],
		"tools":[
			{"name":"submit_form","description":"Submit a form","input_schema":` + xaiZodV4UnionParameters + `},
			{"name":"kubectl_get","description":"Get resources","input_schema":{"type":"object","properties":{"namespace":{"type":"string"}}}}
		]
	}`)
	_, body, _, err := NewXAIExecutor(&config.Config{}).prepareResponsesRequest(
		context.Background(),
		&cliproxyauth.Auth{Provider: "xai"},
		cliproxyexecutor.Request{Model: "grok-4.7", Payload: payload},
		cliproxyexecutor.Options{OriginalRequest: payload, SourceFormat: sdktranslator.FormatClaude},
		false,
	)
	if err != nil {
		t.Fatalf("prepareResponsesRequest returned error: %v", err)
	}
	submit := gjson.GetBytes(body, `tools.#(name=="submit_form").parameters`)
	if !submit.Exists() {
		t.Fatalf("submit_form missing: %s", gjson.GetBytes(body, "tools").Raw)
	}
	assertXAIRootUnionAccepted(t, "submit_form", submit)
	if strings.Contains(submit.Get("oneOf").Raw, `"$ref"`) {
		t.Fatalf("submit_form root union still has $ref branches: %s", submit.Raw)
	}
	if kubectl := gjson.GetBytes(body, `tools.#(name=="kubectl_get").parameters`); !kubectl.Get("properties.namespace").Exists() {
		t.Fatalf("kubectl_get parameters changed: %s", kubectl.Raw)
	}
}

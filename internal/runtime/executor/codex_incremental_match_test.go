package executor

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Items of a two-turn tool conversation, in the shapes each side uses.
const (
	incDeveloper  = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"be brief"}]}`
	incUser1      = `{"type":"message","role":"user","content":[{"type":"input_text","text":"look at the screen"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`
	incReasoning  = `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA"}`
	incOutMessage = `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}`
	incOutCall    = `{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}`
	// What the Chat translator makes of the assistant message the client sends
	// back: the text as one output_text part, the call without id or status.
	incChatMessage = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking."}]}`
	incChatCall    = `{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}`
	incCallOutput  = `{"type":"function_call_output","call_id":"call_1","output":"a.txt"}`
	incUser2       = `{"type":"message","role":"user","content":[{"type":"input_text","text":"and now?"}]}`
)

func incBody(items ...string) []byte {
	return []byte(`{"model":"gpt-5.5","instructions":"","input":[` + strings.Join(items, ",") + `],"tools":[{"type":"function","name":"exec_command"}],"store":false,"stream":true,"prompt_cache_key":"pck-1","client_metadata":{"turn_id":"t1"}}`)
}

func mustParseIncremental(t *testing.T, body []byte) *codexIncrementalRequest {
	t.Helper()
	req, reason := parseCodexIncrementalRequest(body)
	if reason != "" {
		t.Fatalf("parseCodexIncrementalRequest(%s) reason = %q", body, reason)
	}
	return req
}

func incBase(t *testing.T, body []byte, output ...string) *codexIncrementalBase {
	t.Helper()
	req := mustParseIncremental(t, body)
	projection, unusable := projectCodexOutputItems(output)
	return &codexIncrementalBase{items: req.itemDigests(), props: req.props, responseID: "resp_1", output: projection, unusable: unusable}
}

func TestParseCodexIncrementalRequestLocatesInput(t *testing.T) {
	body := incBody(incDeveloper, incUser1)
	req := mustParseIncremental(t, body)
	if got := string(body[req.inputStart:req.inputEnd]); got != "["+incDeveloper+","+incUser1+"]" {
		t.Fatalf("input slice = %s", got)
	}
	if len(req.items) != 2 || req.items[1].raw != incUser1 {
		t.Fatalf("items = %+v", req.items)
	}
	if req.promptCacheKey != "pck-1" || req.hasType {
		t.Fatalf("promptCacheKey=%q hasType=%t", req.promptCacheKey, req.hasType)
	}
	for _, body := range [][]byte{[]byte(`{"input":"hi"}`), []byte(`{"model":"x"}`), []byte(`[1]`)} {
		if _, reason := parseCodexIncrementalRequest(body); reason == "" {
			t.Fatalf("parseCodexIncrementalRequest(%s) took part, want a skip reason", body)
		}
	}
}

func TestCodexIncrementalPropertiesIgnoreTurnFields(t *testing.T) {
	a := mustParseIncremental(t, incBody(incUser1))
	b := mustParseIncremental(t, []byte(strings.Replace(string(incBody(incUser1, incUser2)), `"turn_id":"t1"`, `"turn_id":"t2"`, 1)))
	if a.props != b.props {
		t.Fatal("client_metadata and input changed the properties; they must not")
	}
	c := mustParseIncremental(t, []byte(strings.Replace(string(incBody(incUser1)), `"exec_command"}]`, `"exec_command"},{"type":"function","name":"apply_patch"}]`, 1)))
	if a.props == c.props {
		t.Fatal("a changed tool list kept the properties")
	}
}

func TestMatchCodexIncremental(t *testing.T) {
	first := incBody(incDeveloper, incUser1)
	output := []string{incReasoning, incOutMessage, incOutCall}
	for _, tc := range []struct {
		name   string
		base   *codexIncrementalBase
		body   []byte
		reason string
		added  int
		echoed int
	}{
		{
			name:  "chat client sends back the text and the call",
			base:  incBase(t, first, output...),
			body:  incBody(incDeveloper, incUser1, incChatMessage, incChatCall, incCallOutput),
			added: 1, echoed: 2,
		},
		{
			name:  "official client sends back the output items themselves",
			base:  incBase(t, first, output...),
			body:  incBody(incDeveloper, incUser1, incReasoning, incOutMessage, incOutCall, incCallOutput, incUser2),
			added: 2, echoed: 3,
		},
		{
			name: "several output messages come back as one Chat message",
			base: incBase(t, first, incReasoning,
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Check"}]}`,
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ing."}]}`, incOutCall),
			body:  incBody(incDeveloper, incUser1, incChatMessage, incChatCall, incCallOutput),
			added: 1, echoed: 2,
		},
		{
			name:  "a tool-only reply comes back with an empty Chat message",
			base:  incBase(t, first, incReasoning, incOutCall),
			body:  incBody(incDeveloper, incUser1, `{"type":"message","role":"assistant","content":[]}`, incChatCall, incCallOutput),
			added: 1, echoed: 2,
		},
		{
			name:   "client edited the reply",
			base:   incBase(t, first, output...),
			body:   incBody(incDeveloper, incUser1, strings.Replace(incChatMessage, "Checking.", "Checked.", 1), incChatCall, incCallOutput),
			reason: "echoed_output_mismatch",
		},
		{
			name:   "client dropped the reply",
			base:   incBase(t, first, output...),
			body:   incBody(incDeveloper, incUser1, incUser2),
			reason: "echoed_output_mismatch",
		},
		{
			name:   "different call arguments",
			base:   incBase(t, first, output...),
			body:   incBody(incDeveloper, incUser1, incChatMessage, strings.Replace(incChatCall, "ls", "pwd", 1), incCallOutput),
			reason: "echoed_output_mismatch",
		},
		{
			// A retry after a broken upload replaced the earliest image with a
			// placeholder, so the history no longer starts the same way.
			name:   "history rewritten",
			base:   incBase(t, first, output...),
			body:   incBody(incDeveloper, strings.Replace(incUser1, `{"type":"input_image","image_url":"data:image/png;base64,AAAA"}`, `{"type":"input_text","text":"[An earlier image was left out here to keep this request under its size limit.]"}`, 1), incChatMessage, incChatCall, incCallOutput),
			reason: "input_prefix_mismatch",
		},
		{
			name:   "history shortened",
			base:   incBase(t, first, output...),
			body:   incBody(incUser1),
			reason: "input_shortened",
		},
		{
			name:   "nothing new (a client retry)",
			base:   incBase(t, first, output...),
			body:   incBody(incDeveloper, incUser1, incChatMessage, incChatCall),
			reason: "no_new_input",
		},
		{
			name:   "tools changed",
			base:   incBase(t, first, output...),
			body:   []byte(strings.Replace(string(incBody(incDeveloper, incUser1, incChatMessage, incChatCall, incCallOutput)), `"exec_command"}]`, `"exec_command"},{"type":"function","name":"view_image"}]`, 1)),
			reason: "request_properties_changed",
		},
		{
			name:   "output the projection does not know",
			base:   incBase(t, first, incReasoning, `{"type":"tool_search_call","call_id":"ts_1"}`, incOutMessage),
			body:   incBody(incDeveloper, incUser1, incChatMessage, incUser2),
			reason: "output_item_unsupported:tool_search_call",
		},
		{
			name:  "hosted tool calls are left out on both sides",
			base:  incBase(t, first, incReasoning, `{"type":"web_search_call","id":"ws_1","status":"completed"}`, incOutMessage),
			body:  incBody(incDeveloper, incUser1, incChatMessage, incUser2),
			added: 1, echoed: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match, reason := matchCodexIncremental(tc.base, mustParseIncremental(t, tc.body))
			if reason != tc.reason {
				t.Fatalf("reason = %q, want %q", reason, tc.reason)
			}
			if reason != "" {
				return
			}
			if len(match.newItems) != tc.added || match.echoed != tc.echoed {
				t.Fatalf("new items = %d echoed = %d, want %d and %d", len(match.newItems), match.echoed, tc.added, tc.echoed)
			}
		})
	}
}

func TestBuildCodexIncrementalCreateSendsOnlyNewItems(t *testing.T) {
	cur := mustParseIncremental(t, incBody(incDeveloper, incUser1, incChatMessage, incChatCall, incCallOutput))
	match, reason := matchCodexIncremental(incBase(t, incBody(incDeveloper, incUser1), incReasoning, incOutMessage, incOutCall), cur)
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	msg := buildCodexIncrementalCreate(cur, match.newItems, "resp_1")
	root := gjson.ParseBytes(msg)
	if root.Get("type").String() != "response.create" || root.Get("previous_response_id").String() != "resp_1" {
		t.Fatalf("message = %s", msg)
	}
	if got := root.Get("input").Raw; got != "["+incCallOutput+"]" {
		t.Fatalf("input = %s, want only the new item", got)
	}
	for _, field := range []string{"model", "instructions", "tools", "store", "prompt_cache_key", "client_metadata"} {
		if !root.Get(field).Exists() {
			t.Fatalf("message lost %s: %s", field, msg)
		}
	}
	if size := codexIncrementalCreateSize(cur, match.newItems, "resp_1"); size < len(msg)-16 || size > len(msg)+16 {
		t.Fatalf("estimated size %d, built %d", size, len(msg))
	}
}

func TestBuildCodexFullCreateKeepsTheBody(t *testing.T) {
	body := incBody(incDeveloper, incUser1)
	msg := buildCodexFullCreate(mustParseIncremental(t, body))
	if !gjson.ValidBytes(msg) || gjson.GetBytes(msg, "type").String() != "response.create" {
		t.Fatalf("message = %s", msg)
	}
	if gjson.GetBytes(msg, "input").Raw != gjson.GetBytes(body, "input").Raw || gjson.GetBytes(msg, "previous_response_id").Exists() {
		t.Fatalf("message = %s", msg)
	}
	typed := []byte(`{"type":"x","input":[]}`)
	if got := gjson.GetBytes(buildCodexFullCreate(mustParseIncremental(t, typed)), "type").String(); got != "response.create" {
		t.Fatalf("type = %q", got)
	}
}

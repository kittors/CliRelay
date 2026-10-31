package contentmoderation

import (
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// replaceEverywhere is a tenant policy that rewrites a banned word wherever the
// request has text, tool calls included.
func replaceEverywhere(t *testing.T, mutate func(*WordPolicy), rules ...WordRule) []wordContext {
	t.Helper()
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action = WordActionReplace
		p.ScanScope = WordScanWithTools
		if mutate != nil {
			mutate(p)
		}
	}, rules...)
	return buildWordContexts(nil, tenant, nil)
}

// A tool call's arguments are a JSON document inside a JSON string. A banned
// word in one used to make the whole request unrewritable, so the client got a
// 403 it could not do anything about; now the word is replaced in the document
// and the document is written back into its string.
func TestWordGuardRewritesToolCallArguments(t *testing.T) {
	tests := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		// old and new are the raw JSON of the value that changes: everything
		// else in the request must stay byte for byte as it was.
		old, new string
		replaced int
		// path is where the changed value is, to read it back.
		path string
		// decoded is what that value says once decoded.
		decoded string
	}{
		{
			name:   "chat completions: values, keys and arrays; escapes elsewhere in the string survive",
			format: sdktranslator.FormatOpenAI,
			payload: `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search",` +
				`"arguments":"{\"bad\":\"x\",\"q\":\"a bad \\u003cb\\u003e word\",\"n\":[1,\"bad\"]}"}}]}]}`,
			old:      `"{\"bad\":\"x\",\"q\":\"a bad \\u003cb\\u003e word\",\"n\":[1,\"bad\"]}"`,
			new:      `"{\"***\":\"x\",\"q\":\"a *** \\u003cb\\u003e word\",\"n\":[1,\"***\"]}"`,
			replaced: 3,
			path:     "messages.1.tool_calls.0.function.arguments",
			decoded:  `{"***":"x","q":"a *** \u003cb\u003e word","n":[1,"***"]}`,
		},
		{
			name:   "responses: function_call arguments",
			format: sdktranslator.FormatOpenAIResponse,
			payload: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
				`{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"k\":\"bad\",\"deep\":{\"bad\":true}}"}]}`,
			old:      `"{\"k\":\"bad\",\"deep\":{\"bad\":true}}"`,
			new:      `"{\"k\":\"***\",\"deep\":{\"***\":true}}"`,
			replaced: 2,
			path:     "input.1.arguments",
			decoded:  `{"k":"***","deep":{"***":true}}`,
		},
		{
			name:   "claude: tool_use input, a value and a key",
			format: sdktranslator.FormatClaude,
			payload: `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search",` +
				`"input":{"bad":1,"q":"a bad word","nested":{"bad":["bad"]}}}]}]}`,
			old:      `{"bad":1,"q":"a bad word","nested":{"bad":["bad"]}}`,
			new:      `{"***":1,"q":"a *** word","nested":{"***":["***"]}}`,
			replaced: 4,
			path:     "messages.1.content.0.input",
			decoded:  `{"***":1,"q":"a *** word","nested":{"***":["***"]}}`,
		},
		{
			name:   "claude: a key written with escapes",
			format: sdktranslator.FormatClaude,
			payload: `{"max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n",` +
				`"input":{"b\u0061d":1}}]},{"role":"user","content":"hi"}]}`,
			old:      `{"b\u0061d":1}`,
			new:      `{"***":1}`,
			replaced: 1,
			path:     "messages.0.content.0.input",
			decoded:  `{"***":1}`,
		},
		{
			name:   "gemini: functionCall args",
			format: sdktranslator.FormatGemini,
			payload: `{"contents":[{"role":"user","parts":[{"text":"hi"}]},` +
				`{"role":"model","parts":[{"functionCall":{"name":"f","args":{"bad":"bad","ok":"fine"}}}]}]}`,
			old:      `{"bad":"bad","ok":"fine"}`,
			new:      `{"***":"***","ok":"fine"}`,
			replaced: 2,
			path:     "contents.1.parts.0.functionCall.args",
			decoded:  `{"***":"***","ok":"fine"}`,
		},
		{
			name:   "gemini: functionResponse",
			format: sdktranslator.FormatGemini,
			payload: `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"a bad one"}}}]},` +
				`{"role":"user","parts":[{"text":"hi"}]}]}`,
			old:      `{"result":"a bad one"}`,
			new:      `{"result":"a *** one"}`,
			replaced: 1,
			path:     "contents.0.parts.0.functionResponse.response",
			decoded:  `{"result":"a *** one"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Count(tt.payload, tt.old) != 1 {
				t.Fatalf("test data: %q must appear once in the payload", tt.old)
			}
			contexts := replaceEverywhere(t, nil, kw("bad"))
			verdict := evalOne(t, tt.format, tt.payload, contexts)
			if verdict.Blocked || verdict.Action != WordVerdictReplace || verdict.Replaced != tt.replaced {
				t.Fatalf("verdict = %+v", verdict)
			}
			want := strings.Replace(tt.payload, tt.old, tt.new, 1)
			if string(verdict.Payload) != want {
				t.Fatalf("\n got  %s\n want %s", verdict.Payload, want)
			}
			if !gjson.ValidBytes(verdict.Payload) {
				t.Fatalf("not valid JSON: %s", verdict.Payload)
			}
			// Read the changed value back the way a client would: the arguments of
			// a call are a string that holds JSON, the others are JSON already.
			value := gjson.GetBytes(verdict.Payload, tt.path)
			got := value.Raw
			if value.Type == gjson.String {
				got = value.String()
				if !gjson.Valid(got) {
					t.Fatalf("%s no longer holds JSON: %s", tt.path, got)
				}
			}
			if got != tt.decoded {
				t.Fatalf("%s = %s, want %s", tt.path, got, tt.decoded)
			}
			// What the rewrite wrote is clean: the same policy finds nothing in it.
			if again := evalOne(t, tt.format, string(verdict.Payload), contexts); again.Action != WordVerdictAllow {
				t.Fatalf("rewritten request still hits: %+v", again)
			}
		})
	}
}

// Text, keys and the JSON inside arguments are all in the same request: one
// rewrite handles them together.
func TestWordGuardRewritesTextKeysAndArgumentsTogether(t *testing.T) {
	payload := `{"model":"m","messages":[{"role":"user","content":"a bad start"},` +
		`{"role":"assistant","content":"bad middle","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"bad\":\"bad\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c","content":"result bad"},{"role":"user","content":"the end"}]}`
	want := `{"model":"m","messages":[{"role":"user","content":"a *** start"},` +
		`{"role":"assistant","content":"*** middle","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{\"***\":\"***\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c","content":"result ***"},{"role":"user","content":"the end"}]}`
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, replaceEverywhere(t, nil, kw("bad")))
	if verdict.Blocked || verdict.Replaced != 5 || string(verdict.Payload) != want {
		t.Fatalf("verdict = %+v\n got  %s\n want %s", verdict, verdict.Payload, want)
	}
}

// Two platform and tenant replacements that meet in a tool call's arguments are
// merged the way they are in plain text.
func TestWordGuardLayersMeetInsideArguments(t *testing.T) {
	platform := policyOf(t, WordScopePlatform, "", func(p *WordPolicy) {
		p.Action, p.ScanScope, p.Replacement = WordActionReplace, WordScanWithTools, "<P>"
	}, kw("foo bar"))
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.ScanScope, p.Replacement = WordActionReplace, WordScanWithTools, "<T>"
	}, kw("bar baz"))
	payload := `{"messages":[{"role":"assistant","content":"x","tool_calls":[{"function":{"name":"f","arguments":"{\"q\":\"x foo bar baz y\"}"}}]},{"role":"user","content":"hi"}]}`
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, buildWordContexts(platform, tenant, nil))
	want := `{"messages":[{"role":"assistant","content":"x","tool_calls":[{"function":{"name":"f","arguments":"{\"q\":\"x <P> y\"}"}}]},{"role":"user","content":"hi"}]}`
	if string(verdict.Payload) != want {
		t.Fatalf("payload = %s\n want    %s", verdict.Payload, want)
	}
}

// Two keys of one object that come out the same would leave a value that nothing
// can reach (a JSON parser keeps the last one), so a name that is taken gets a
// number.
func TestWordGuardRenamedKeysStayDistinct(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		// keys is how many distinct keys the object has afterwards.
		keys int
	}{
		{"two banned keys", `{"bad":1,"BAD":2}`, `{"***":1,"***_2":2}`, 2},
		{"a key that is already the replacement", `{"***":0,"bad":1}`, `{"***":0,"***_2":1}`, 2},
		{"the replacement comes later in the object", `{"bad":1,"***":0}`, `{"***_2":1,"***":0}`, 2},
		{"three of them", `{"bad":1,"***":0,"BAD":2,"bad":3}`, `{"***_2":1,"***":0,"***_3":2,"***_4":3}`, 4},
		{"separate objects do not share names", `{"bad":{"bad":1},"x":{"bad":2}}`, `{"***":{"***":1},"x":{"***":2}}`, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := `{"max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":` + tt.input + `}]},{"role":"user","content":"hi"}]}`
			verdict := evalOne(t, sdktranslator.FormatClaude, payload, replaceEverywhere(t, nil, kw("bad")))
			if verdict.Blocked || verdict.Payload == nil {
				t.Fatalf("verdict = %+v", verdict)
			}
			got := gjson.GetBytes(verdict.Payload, "messages.0.content.0.input").Raw
			if got != tt.want {
				t.Fatalf("input = %s, want %s", got, tt.want)
			}
			// Nothing was lost: every key is still there, each with its own value.
			distinct := make(map[string]bool)
			gjson.Parse(got).ForEach(func(key, _ gjson.Result) bool { distinct[key.String()] = true; return true })
			if len(distinct) != tt.keys {
				t.Fatalf("%d distinct keys survive in %s, want %d", len(distinct), got, tt.keys)
			}
		})
	}
}

// The replacement text may need escaping when it becomes a key.
func TestWordGuardRenamedKeyIsEscaped(t *testing.T) {
	payload := `{"max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{"bad":1}}]},{"role":"user","content":"hi"}]}`
	contexts := replaceEverywhere(t, func(p *WordPolicy) { p.Replacement = `[x "y"]` }, kw("bad"))
	verdict := evalOne(t, sdktranslator.FormatClaude, payload, contexts)
	if verdict.Blocked || verdict.Payload == nil || !gjson.ValidBytes(verdict.Payload) {
		t.Fatalf("verdict = %+v", verdict)
	}
	if got := gjson.GetBytes(verdict.Payload, "messages.0.content.0.input.@keys").Raw; got != `["[x \"y\"]"]` {
		t.Fatalf("keys = %s", got)
	}
}

// Claude's thinking blocks and Gemini's thought parts are signed: the model
// checks the signature against the text it sent, and a different text is
// refused upstream. They are still read, so a banned word in one is recorded,
// but the request goes on with the block as the client sent it.
func TestWordGuardSignedThinkingIsRecordedNotRewritten(t *testing.T) {
	claude := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"a bad thought","signature":"SIG=="},{"type":"text","text":"fine"}]},` +
		`{"role":"user","content":"go on"}]}`
	gemini := `{"contents":[{"role":"user","parts":[{"text":"hi"}]},` +
		`{"role":"model","parts":[{"text":"a bad thought","thought":true,"thoughtSignature":"SIG"},{"text":"fine"}]},` +
		`{"role":"user","parts":[{"text":"go on"}]}]}`
	for _, tt := range []struct {
		name    string
		format  sdktranslator.Format
		payload string
		// plain is a request with a banned word in ordinary text as well.
		plain, plainWant string
	}{
		{"claude thinking", sdktranslator.FormatClaude, claude,
			strings.Replace(claude, `"go on"`, `"go bad on"`, 1), strings.Replace(claude, `"go on"`, `"go *** on"`, 1)},
		{"gemini thought", sdktranslator.FormatGemini, gemini,
			strings.Replace(gemini, `"go on"`, `"go bad on"`, 1), strings.Replace(gemini, `"go on"`, `"go *** on"`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			contexts := replaceEverywhere(t, nil, kw("bad"))

			// Recorded: the hit is in the log, as a log, and the request is as it was.
			verdict := evalOne(t, tt.format, tt.payload, contexts)
			if verdict.Blocked || verdict.Action != WordVerdictLog || verdict.Payload != nil || verdict.Replaced != 0 || verdict.MatchCount != 1 {
				t.Fatalf("verdict = %+v", verdict)
			}
			if len(verdict.Matches) != 1 || verdict.Matches[0].Action != WordActionLog || verdict.Matches[0].Pattern != "bad" {
				t.Fatalf("matches = %+v", verdict.Matches)
			}

			// A word elsewhere in the same request is replaced; the signed block is not.
			verdict = evalOne(t, tt.format, tt.plain, contexts)
			if verdict.Blocked || verdict.Action != WordVerdictReplace || verdict.Replaced != 1 || verdict.MatchCount != 2 {
				t.Fatalf("verdict = %+v", verdict)
			}
			if string(verdict.Payload) != tt.plainWant {
				t.Fatalf("\n got  %s\n want %s", verdict.Payload, tt.plainWant)
			}

			// Block still blocks: a banned word is not let through because it is signed.
			blocking := buildWordContexts(nil, policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
				p.Action, p.ScanScope = WordActionBlock, WordScanWithTools
			}, kw("bad")), nil)
			if verdict = evalOne(t, tt.format, tt.payload, blocking); !verdict.Blocked || verdict.Action != WordVerdictBlock {
				t.Fatalf("verdict = %+v", verdict)
			}
		})
	}
}

// A request that cannot be rewritten in full is refused whole: a partly
// rewritten request would carry the banned word on without the client knowing.
func TestWordGuardUnrewritableTextRefusesTheWholeRequest(t *testing.T) {
	payload := "{\"messages\":[{\"role\":\"user\",\"content\":\"a bad start\"},{\"role\":\"user\",\"content\":\"b \xff bad\"}]}"
	verdict := evalOne(t, sdktranslator.FormatOpenAI, payload, replaceEverywhere(t, nil, kw("bad")))
	if !verdict.Blocked || verdict.Payload != nil || verdict.Reason != WordReasonUnrewritable || verdict.Field != "messages[1].content" {
		t.Fatalf("verdict = %+v", verdict)
	}
	// The client is told where; the rule is not in the message.
	if !strings.Contains(verdict.BlockMessage, "messages[1].content") || strings.Contains(verdict.BlockMessage, "bad") {
		t.Fatalf("block message = %q", verdict.BlockMessage)
	}
}

// The path of a key says it is a key, so the client knows to rename it.
func TestWordGuardUnrewritableKeyIsNamedAsAKey(t *testing.T) {
	payload := "{\"max_tokens\":5,\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"n\",\"input\":{\"ok\":1,\"x \xff bad\":2}}]},{\"role\":\"user\",\"content\":\"hi\"}]}"
	verdict := evalOne(t, sdktranslator.FormatClaude, payload, replaceEverywhere(t, nil, kw("bad")))
	want := `messages[0].content[0].input["x \xff bad"] (an object key)`
	if !verdict.Blocked || verdict.Reason != WordReasonUnrewritable || verdict.Field != want {
		t.Fatalf("verdict = %+v\n want field %s", verdict, want)
	}
}

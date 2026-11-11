package executor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Strings that stand for everything a request can carry and the shape must never
// repeat: text, ids, arguments, names, ciphertext and pixels. Every fixture below
// plants them, and the shape's JSON is searched for each.
var shapeSecrets = []string{
	"SECRET-USER-TEXT", "SECRET-ARGUMENT", "SECRET-OUTPUT-TEXT", "call_SECRET_ID", "item_SECRET_ID",
	"SECRET-DEVELOPER-TEXT", "SECRET-TYPE-NAME", "SECRET-ROLE", "SECRET_UNKNOWN_KEY", "SECRET-INSTRUCTIONS",
	"SECRET-CACHE-KEY", "SECRET_TOOL_NAME", "SECRET-SCHEMA-TEXT", "SESSION-SECRET-ID", "CONV-SECRET-ID",
	"SECRET-TURN-ID", "SECRET-META-VALUE", "SECRET-TOOL-CHOICE", "SECRET-REASONING-TEXT",
	// Runs of the filler the ciphertext and image payloads are made of.
	"AAAAAAAA", "CCCCCCCC", "BBBBBBBB", "JJJJJJJJ", "DDDDDDDD", "EEEEEEEE", "base64", "data:image",
}

func assertNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range shapeSecrets {
		if strings.Contains(raw, secret) {
			t.Fatalf("the shape repeats %q:\n%s", secret, raw)
		}
	}
}

// shapeItem is one input item of the fixture and the bucket it must be counted in.
type shapeItem struct {
	raw    string
	bucket string
}

var (
	shapeReasoningCipher  = "gAAAA" + strings.Repeat("A", 100) // 105 bytes
	shapeCompactionCipher = "gAAAA" + strings.Repeat("C", 295) // 300 bytes
)

// liteShapeFixture is a Responses Lite request: tools inside an additional_tools
// item, reasoning and compaction ciphertext, images in a message and in a tool
// output, data URLs inside text, and names and values that must not survive.
func liteShapeFixture(turnMetadata string) ([]byte, []shapeItem) {
	pngURL := "data:image/png;base64," + strings.Repeat("B", 400)   // 422 bytes
	jpegURL := "data:image/jpeg;base64," + strings.Repeat("J", 600) // 623 bytes
	markdown := "![SECRET-USER-TEXT](data:image/png;base64," + strings.Repeat("D", 300) + ") and ![x](data:image/png;base64," + strings.Repeat("E", 300) + ")"
	items := []shapeItem{
		{`{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET-USER-TEXT"}]}`, "message:user"},
		{`{"type":"reasoning","id":"item_SECRET_ID","summary":[],"encrypted_content":"` + shapeReasoningCipher + `"}`, "reasoning"},
		{`{"type":"reasoning","id":"item_SECRET_ID","summary":[],"encrypted_content":"` + shapeReasoningCipher + `"}`, "reasoning"},
		{`{"type":"function_call","call_id":"call_SECRET_ID","name":"SECRET_TOOL_NAME","arguments":"{\"cmd\":\"SECRET-ARGUMENT\"}"}`, "function_call"},
		{`{"type":"function_call_output","call_id":"call_SECRET_ID","output":"SECRET-OUTPUT-TEXT"}`, "function_call_output"},
		{`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + mustJSONString(markdown) + `}]}`, "message:assistant"},
		{`{"type":"reasoning","summary":[{"type":"summary_text","text":"SECRET-REASONING-TEXT"}],"encrypted_content":"` + shapeReasoningCipher + `"}`, "reasoning"},
		{`{"type":"compaction","encrypted_content":"` + shapeCompactionCipher + `"}`, "compaction"},
		{`{"type":"additional_tools","tools":[{"type":"function","name":"SECRET_TOOL_NAME","parameters":{"description":"SECRET-SCHEMA-TEXT"}},{"type":"function","name":"b"},{"type":"namespace","name":"c","tools":[{"type":"function","name":"d"}]}]}`, "additional_tools"},
		{`{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET-USER-TEXT"},{"type":"input_image","image_url":"` + pngURL + `"}]}`, "message:user"},
		{`{"type":"function_call_output","call_id":"call_SECRET_ID","output":[{"type":"input_image","image_url":"` + jpegURL + `"}]}`, "function_call_output"},
		{`{"type":"message","role":"developer","content":"SECRET-DEVELOPER-TEXT"}`, "message:developer"},
		{`{"type":"SECRET-TYPE-NAME","id":"item_SECRET_ID"}`, "other"},
		{`{"type":"message","role":"SECRET-ROLE","content":[{"type":"input_text","text":"SECRET-USER-TEXT"}]}`, "message:other"},
		{`{"role":"user","content":"typeless SECRET-USER-TEXT"}`, "message:user"},
	}
	raws := make([]string, len(items))
	for i, item := range items {
		raws[i] = item.raw
	}
	metadata := `{"ws_request_header_x_openai_internal_codex_responses_lite":"true","x-codex-turn-metadata":` + mustJSONString(turnMetadata) + `}`
	body := `{"model":"gpt-6-astra","instructions":"SECRET-INSTRUCTIONS and a longer text","input":[` + strings.Join(raws, ",") + `],` +
		`"tools":[{"type":"function","name":"SECRET_TOOL_NAME"},{"type":"web_search"}],"tool_choice":"auto","store":false,"stream":true,` +
		`"reasoning":{"effort":"high","summary":"auto"},"include":["reasoning.encrypted_content"],"service_tier":"priority",` +
		`"prompt_cache_key":"SECRET-CACHE-KEY","client_metadata":` + metadata + `,"SECRET_UNKNOWN_KEY":1}`
	return []byte(body), items
}

func TestUpstreamShapeCountsALiteRequest(t *testing.T) {
	body, items := liteShapeFixture("{}")
	shape := &codexUpstreamShape{}
	shape.scanBody(body)

	if shape.Bytes.Plain != len(body) {
		t.Fatalf("plain bytes = %d, want %d", shape.Bytes.Plain, len(body))
	}
	if shape.Input.Items != len(items) {
		t.Fatalf("input items = %d, want %d", shape.Input.Items, len(items))
	}
	wantTypes := map[string]int{}
	largest, largestBucket := 0, ""
	for _, item := range items {
		wantTypes[item.bucket]++
		if len(item.raw) > largest {
			largest, largestBucket = len(item.raw), item.bucket
		}
	}
	if !reflect.DeepEqual(shape.Input.ByType, wantTypes) {
		t.Fatalf("by_type = %v, want %v", shape.Input.ByType, wantTypes)
	}
	if shape.Input.LargestItemBytes != largest || shape.Input.LargestItemType != largestBucket {
		t.Fatalf("largest item = %s/%d, want %s/%d", shape.Input.LargestItemType, shape.Input.LargestItemBytes, largestBucket, largest)
	}
	if want := map[string]codexShapeCipher{"reasoning": {N: 3, Bytes: 3 * len(shapeReasoningCipher)}, "compaction": {N: 1, Bytes: len(shapeCompactionCipher)}}; !reflect.DeepEqual(shape.Ciphertext, want) {
		t.Fatalf("ciphertext = %v, want %v", shape.Ciphertext, want)
	}
	if want := (codexShapeTools{TopLevel: 2, ByType: map[string]int{"function": 1, "web_search": 1}, AdditionalToolsItems: 1, AdditionalToolsTools: 3}); !reflect.DeepEqual(shape.Tools, want) {
		t.Fatalf("tools = %+v, want %+v", shape.Tools, want)
	}
	// Two input_image parts, one in a message and one in a tool output; the two
	// markdown data URLs in the assistant's text are not input_image parts.
	if want := (codexShapeImages{InputImage: 2, InputImageBytes: 422 + 623, LargestBytes: 623, DataURLInText: 2}); shape.Images != want {
		t.Fatalf("images = %+v, want %+v", shape.Images, want)
	}

	request := shape.Request
	wantKeys := []string{"client_metadata", "include", "input", "instructions", "model", "prompt_cache_key", "reasoning", "service_tier", "store", "stream", "tool_choice", "tools"}
	if !reflect.DeepEqual(request.Keys, wantKeys) || request.OtherKeys != 1 {
		t.Fatalf("keys = %v (+%d other), want %v (+1)", request.Keys, request.OtherKeys, wantKeys)
	}
	if request.Store == nil || *request.Store || request.Stream == nil || !*request.Stream {
		t.Fatalf("store/stream = %v/%v, want false/true", request.Store, request.Stream)
	}
	if !request.ReasoningEffort || request.ToolChoice != "auto" || request.InstructionsBytes != len("SECRET-INSTRUCTIONS and a longer text") {
		t.Fatalf("request = %+v", request)
	}
	if !shape.lite {
		t.Fatal("client_metadata marks the request Lite")
	}

	raw := string(shape.marshalWithinBudget())
	assertNoSecrets(t, raw)
	if len(raw) > codexShapeBudgetBytes {
		t.Fatalf("shape is %d bytes, budget %d", len(raw), codexShapeBudgetBytes)
	}
	// An ordinary request fits as built: nothing is cut for it.
	whole, err := json.Marshal(shape)
	if err != nil || string(whole) != raw {
		t.Fatalf("an ordinary shape must not be trimmed:\n%s\n%s", whole, raw)
	}
}

func TestUpstreamShapeDistinguishesAnAbsentFieldFromAFalseOne(t *testing.T) {
	shape := &codexUpstreamShape{}
	shape.scanBody([]byte(`{"model":"gpt-5.4","input":"hi","reasoning":{"summary":"auto"},"tool_choice":{"type":"function","name":"SECRET-TOOL-CHOICE"}}`))
	if shape.Request.Store != nil || shape.Request.Stream != nil {
		t.Fatal("store and stream were not sent")
	}
	if shape.Request.ReasoningEffort {
		t.Fatal("a reasoning object without an effort has none")
	}
	if shape.Request.ToolChoice != "function" {
		t.Fatalf("tool_choice type = %q", shape.Request.ToolChoice)
	}
	if shape.Input.StringBytes != 2 || shape.Input.Items != 0 {
		t.Fatalf("a string input is measured, not counted: %+v", shape.Input)
	}
	assertNoSecrets(t, string(shape.marshalWithinBudget()))
}

// Names come from the caller. Only ones that fit the alphabet are kept, and a
// caller cannot grow the shape by inventing more.
func TestUpstreamShapeRefusesNamesOutsideItsAlphabet(t *testing.T) {
	var types, tools []string
	for i := 0; i < 40; i++ {
		name := string(rune('a'+i/26)) + string(rune('a'+i%26))
		types = append(types, `{"type":"`+name+`"}`)
		tools = append(tools, `{"type":"`+name+`"}`)
	}
	hostile := []string{
		`{"type":"Message"}`, `{"type":"SECRET-TYPE-NAME"}`, `{"type":"item1"}`, `{"type":""}`, `{"type":7}`, `{}`, `"SECRET-ROLE"`, `7`,
		`{"type":"` + strings.Repeat("a", 41) + `"}`,
	}
	body := `{"input":[` + strings.Join(append(types, hostile...), ",") + `],"tools":[` + strings.Join(append(tools, hostile...), ",") + `],"tool_choice":{"type":"SECRET-TOOL-CHOICE"},"SECRET_UNKNOWN_KEY":1}`
	shape := &codexUpstreamShape{}
	shape.scanBody([]byte(body))

	for name, counts := range map[string]map[string]int{"input": shape.Input.ByType, "tools": shape.Tools.ByType} {
		if len(counts) > codexShapeMaxTypes+1 {
			t.Fatalf("%s keeps %d distinct names, want at most %d and an other bucket", name, len(counts), codexShapeMaxTypes)
		}
		total := 0
		for key, n := range counts {
			if key != codexShapeOther && codexShapeName(key) != key {
				t.Fatalf("%s kept the name %q", name, key)
			}
			total += n
		}
		if want := 40 + len(hostile); total != want {
			t.Fatalf("%s counts %d, want every one of the %d counted", name, total, want)
		}
		if counts[codexShapeOther] < len(hostile)+16 {
			t.Fatalf("%s other bucket = %d, want the %d refused names and the %d beyond the cap", name, counts[codexShapeOther], len(hostile), 16)
		}
	}
	if shape.Request.ToolChoice != codexShapeOther || shape.Request.OtherKeys != 1 {
		t.Fatalf("tool_choice = %q, other keys = %d", shape.Request.ToolChoice, shape.Request.OtherKeys)
	}
	assertNoSecrets(t, string(shape.marshalWithinBudget()))
}

func TestCodexShapeName(t *testing.T) {
	for name, want := range map[string]string{
		"message": "message", "function_call_output": "function_call_output", "message:user": "message:user", "a": "a",
		strings.Repeat("a", 40): strings.Repeat("a", 40),
		"":                      "other", "Message": "other", "item1": "other", "a-b": "other", "a b": "other", "é": "other",
		strings.Repeat("a", 41): "other",
	} {
		if got := codexShapeName(name); got != want {
			t.Errorf("codexShapeName(%q) = %q, want %q", name, got, want)
		}
	}
}

// The scan reads the body through a view, not a copy; what it keeps must not point
// into it, or a retained shape would pin a request of tens of megabytes and read
// whatever the buffer holds next.
func TestUpstreamShapeDoesNotPointIntoTheBody(t *testing.T) {
	body, _ := liteShapeFixture(`{"trigger":"auto"}`)
	body = append([]byte(nil), body...)
	shape := &codexUpstreamShape{}
	shape.scanBody(body)
	before := string(shape.marshalWithinBudget())
	for i := range body {
		body[i] = 'X'
	}
	if after := string(shape.marshalWithinBudget()); after != before {
		t.Fatalf("the shape changed when the body did:\nbefore %s\nafter  %s", before, after)
	}
}

func TestUpstreamShapeSurvivesBodiesThatAreNotRequests(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"array":        `[1,2,3]`,
		"scalar":       `7`,
		"null":         `null`,
		"truncated":    `{"input":[{"type":"mes`,
		"input scalar": `{"input":5,"tools":"x","reasoning":3,"tool_choice":4,"client_metadata":"x"}`,
		"not json":     `<html>bad gateway</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			shape := &codexUpstreamShape{}
			shape.scanBody([]byte(body))
			if shape.Bytes.Plain != len(body) {
				t.Fatalf("plain bytes = %d", shape.Bytes.Plain)
			}
			if raw := shape.marshalWithinBudget(); !json.Valid(raw) {
				t.Fatalf("shape is not JSON: %s", raw)
			}
		})
	}
}

func TestCodexShapeClientReadsTurnMetadata(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	started := now.Add(-160*time.Hour - 48*time.Minute)
	windowID := fmt.Sprintf("%08x-%04x-7000-8000-000000000000", uint64(started.UnixMilli())>>16, uint64(started.UnixMilli())&0xffff)
	metadata := `{"request_kind":"compaction","window_number":1,"history_initialization":"cold_resume","turn_trigger":"automation_heartbeat_scheduled",` +
		`"source":"app-server","context_window_id":"` + windowID + `","trigger":"auto","reason":"context_limit","strategy":"memento","implementation":"responses",` +
		`"session_id":"SESSION-SECRET-ID","turn_id":"SECRET-TURN-ID","other_field":"SECRET-META-VALUE"}`

	client := codexShapeClientFrom(http.Header{"X-Codex-Turn-Metadata": {metadata}}, nil, now)
	age := 160.8
	window := 1
	want := codexShapeClient{
		RequestKind: "compaction", WindowNumber: &window, HistoryInitialization: "cold_resume", TurnTrigger: "automation_heartbeat_scheduled",
		Source: "app-server", ContextWindowAgeHours: &age,
		Extra: map[string]string{"trigger": "auto", "reason": "context_limit", "strategy": "memento", "implementation": "responses"},
	}
	if !reflect.DeepEqual(client, want) {
		t.Fatalf("client = %+v, want %+v", client, want)
	}
	raw, _ := json.Marshal(client)
	assertNoSecrets(t, string(raw))

	// Without the client's own header the copy that went out is read instead.
	if got := codexShapeClientFrom(nil, http.Header{"X-Codex-Turn-Metadata": {metadata}}, now); got.RequestKind != "compaction" {
		t.Fatalf("fallback to the sent header failed: %+v", got)
	}
	if got := codexShapeClientFrom(nil, nil, now); !reflect.DeepEqual(got, codexShapeClient{}) {
		t.Fatalf("no metadata should read as nothing, got %+v", got)
	}
}

func TestCodexShapeClientRefusesValuesThatAreNotEnums(t *testing.T) {
	metadata := `{"request_kind":"Compaction SECRET-META-VALUE","turn_trigger":"` + strings.Repeat("a", 60) + `","history_initialization":7,"source":"","window_number":"1","context_window_id":"SECRET-META-VALUE","reason":{"x":1}}`
	client := codexShapeClientFrom(http.Header{"X-Codex-Turn-Metadata": {metadata}}, nil, time.Now())
	want := codexShapeClient{RequestKind: "other", TurnTrigger: "other", Source: "other"}
	if !reflect.DeepEqual(client, want) {
		t.Fatalf("client = %+v, want %+v", client, want)
	}
	raw, _ := json.Marshal(client)
	assertNoSecrets(t, string(raw))
}

func TestCodexUUIDv7AgeHours(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	v7 := func(at time.Time) string {
		return fmt.Sprintf("%08x-%04x-7abc-8def-0123456789ab", uint64(at.UnixMilli())>>16, uint64(at.UnixMilli())&0xffff)
	}
	if age, ok := codexUUIDv7AgeHours(v7(now.Add(-90*time.Minute)), now); !ok || age != 1.5 {
		t.Fatalf("age = %v, %v; want 1.5", age, ok)
	}
	if age, ok := codexUUIDv7AgeHours(v7(now.Add(-24*time.Hour)), now); !ok || age != 24 {
		t.Fatalf("age = %v, %v; want 24", age, ok)
	}
	for name, id := range map[string]string{
		"a v4 id":    "0a1b2c3d-4e5f-4a6b-8c7d-0123456789ab",
		"too short":  "0a1b2c3d-4e5f-7a6b",
		"not hex":    "zzzzzzzz-zzzz-7zzz-8zzz-zzzzzzzzzzzz",
		"future":     v7(now.Add(time.Hour)),
		"misplaced":  "0a1b2c3d4e5f-7a6b-8c7d-0123456789ab-",
		"empty":      "",
		"unexpected": "SECRET-META-VALUE-SECRET-META-VALUE-SEC",
	} {
		if age, ok := codexUUIDv7AgeHours(id, now); ok {
			t.Errorf("%s: age = %v, want none", name, age)
		}
	}
}

func TestCodexShapeSentHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Version", "0.167.1")
	h.Set("User-Agent", "Codex Desktop/0.167.1-alpha.3.2 (Windows 10; x86_64)")
	h.Set("X-Codex-Window-Id", "SESSION-SECRET-ID:0")
	h.Set("X-Codex-Routing-Hint", "model=gpt-6-astra")
	h.Set("Content-Encoding", "zstd")
	h.Set("Session-Id", "SESSION-SECRET-ID")
	h.Set("Thread-Id", "SESSION-SECRET-ID")
	h.Set("X-Client-Request-Id", "SESSION-SECRET-ID")
	h.Set("Conversation_id", "CONV-SECRET-ID")

	got := codexShapeSentHeaders(h)
	window, yes, no := 0, true, false
	want := codexShapeHeaders{
		Version: "0.167.1", UAVersion: "0.167.1", WindowCounter: &window, RoutingHint: true, ContentEncoding: "zstd",
		SessionEqualsThread: &yes, SessionEqualsConversation: &no, SessionEqualsClientRequest: &yes,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent headers = %+v, want %+v", got, want)
	}
	raw, _ := json.Marshal(got)
	assertNoSecrets(t, string(raw))

	// The Lite header counts only when it went out, and ids on one side only say nothing.
	h = http.Header{}
	h.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	h.Set("Session_id", "SESSION-SECRET-ID")
	got = codexShapeSentHeaders(h)
	if !got.LiteHeader || got.RoutingHint || got.SessionEqualsThread != nil || got.SessionEqualsConversation != nil || got.WindowCounter != nil {
		t.Fatalf("sent headers = %+v", got)
	}
}

func TestCodexShapeHeaderValuesAreFiltered(t *testing.T) {
	h := http.Header{}
	h.Set("Version", "SECRET META VALUE with spaces")
	h.Set("User-Agent", "codex_cli_rs/9.9.9 SECRET-META-VALUE")
	h.Set("X-Codex-Window-Id", "SESSION-SECRET-ID:abc")
	h.Set("Content-Encoding", "SECRET-META-VALUE")
	got := codexShapeSentHeaders(h)
	if got.Version != "" || got.WindowCounter != nil || got.ContentEncoding != "other" {
		t.Fatalf("sent headers = %+v", got)
	}
	if got.UAVersion != "9.9.9" {
		t.Fatalf("a version taken from the user agent is digits and dots only, got %q", got.UAVersion)
	}
	raw, _ := json.Marshal(got)
	assertNoSecrets(t, string(raw))
	if codexShapeVersion(strings.Repeat("1", 33)) != "" || codexShapeVersion("1.2.3-alpha.4+build_5") == "" {
		t.Fatal("version filter")
	}
}

func TestBuildCodexUpstreamShapeSizesAndEdits(t *testing.T) {
	body, _ := liteShapeFixture(`{"request_kind":"compaction"}`)
	req, err := http.NewRequest(http.MethodPost, codexResponsesURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "zstd")
	req.ContentLength = 251234
	client := http.Header{}
	client.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	client.Set("X-Codex-Turn-Metadata", `{"request_kind":"turn"}`)

	edits := &codexBodyEdits{IDsStripped: 360, ForeignReasoningDropped: 2}
	shape := buildCodexUpstreamShape(codexShapeSource{Plain: body, Request: req, ClientHeaders: client, Edits: edits, Retried: true, Now: time.Now()})
	if shape.Bytes.Plain != len(body) || shape.Bytes.Zstd != 251234 {
		t.Fatalf("bytes = %+v", shape.Bytes)
	}
	if shape.Client.RequestKind != "turn" || !shape.Client.Lite {
		t.Fatalf("client = %+v: the client's own header is the one read, and it says Lite", shape.Client)
	}
	if shape.SentHeaders.LiteHeader {
		t.Fatal("the gateway did not forward the Lite header on this request")
	}
	want := codexBodyEdits{IDsStripped: 360, ForeignReasoningDropped: 2, ReasoningRetry: true}
	if shape.Mutations == nil || *shape.Mutations != want {
		t.Fatalf("mutations = %+v, want %+v", shape.Mutations, want)
	}
	if *edits != (codexBodyEdits{IDsStripped: 360, ForeignReasoningDropped: 2}) {
		t.Fatal("building the shape must not write into the attempt's own tally")
	}

	// A request nothing rewrote and nothing retried has no mutations to report.
	if clean := buildCodexUpstreamShape(codexShapeSource{Plain: body, Now: time.Now()}); clean.Mutations != nil || clean.Bytes.Zstd != 0 {
		t.Fatalf("clean shape = %+v", clean)
	}
}

// A request that uses every type, key, tool and metadata field the shape tracks
// still fits the budget; the cuts take the detail that says least.
func TestUpstreamShapeStaysWithinItsBudgetInTheWorstCase(t *testing.T) {
	var items, tools []string
	for i := 0; i < 60; i++ {
		name := string(rune('a'+i/26)) + string(rune('a'+i%26))
		items = append(items, `{"type":"`+name+`","encrypted_content":"gAAAAxx"}`)
		tools = append(tools, `{"type":"`+name+`"}`)
	}
	root := map[string]any{}
	for key := range codexShapeKnownKeys {
		root[key] = 1
	}
	root["input"] = json.RawMessage("[" + strings.Join(items, ",") + "]")
	root["tools"] = json.RawMessage("[" + strings.Join(tools, ",") + "]")
	root["store"], root["stream"] = true, false
	root["reasoning"] = map[string]any{"effort": "high"}
	root["tool_choice"] = map[string]any{"type": "function"}
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}

	sent := http.Header{}
	sent.Set("Version", strings.Repeat("9", 32))
	sent.Set("User-Agent", "codex_cli_rs/"+strings.Repeat("9.", 12))
	sent.Set("X-Codex-Window-Id", "id:999999999")
	sent.Set("X-Codex-Routing-Hint", "x")
	sent.Set("Content-Encoding", "zstd")
	sent.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	sent.Set("Session-Id", "a")
	sent.Set("Thread-Id", "a")
	sent.Set("Conversation_id", "b")
	sent.Set("X-Client-Request-Id", "a")
	req := &http.Request{Header: sent, ContentLength: 999_999_999}
	long := strings.Repeat("a", 40)
	client := http.Header{}
	client.Set("X-Codex-Turn-Metadata", `{"request_kind":"`+long+`","window_number":999999999,"history_initialization":"`+long+`","turn_trigger":"`+long+`","source":"`+long+
		`","context_window_id":"018c5a5e-1234-7000-8000-000000000000","trigger":"`+long+`","reason":"`+long+`","strategy":"`+long+`","implementation":"`+long+`"}`)
	big := 999_999_999
	edits := &codexBodyEdits{
		ImageBridgeTool: true, ToolChoiceForced: true, EffortDropped: true, IDsStripped: big, ShellsDropped: big, DataURLsReplaced: big,
		ImageResultsDropped: big, ImageReattached: true, ForeignReasoningDropped: big,
	}

	shape := buildCodexUpstreamShape(codexShapeSource{Plain: body, Request: req, ClientHeaders: client, Edits: edits, Retried: true, Now: time.Now()})
	whole, _ := json.Marshal(shape)
	if len(whole) <= codexShapeBudgetBytes {
		t.Fatalf("the worst case no longer exceeds the budget (%d bytes); make the fixture worse so the cuts stay tested", len(whole))
	}
	raw := shape.marshalWithinBudget()
	if len(raw) > codexShapeBudgetBytes || !json.Valid(raw) {
		t.Fatalf("shape is %d bytes (budget %d), valid=%v:\n%s", len(raw), codexShapeBudgetBytes, json.Valid(raw), raw)
	}
	// What a comparison needs first survives every cut.
	for _, path := range []string{"bytes.plain", "input.items", "mutations.reasoning_retry", "mutations.foreign_reasoning_dropped", "sent_headers.content_encoding", "client.request_kind"} {
		if !gjson.GetBytes(raw, path).Exists() {
			t.Errorf("%s was cut:\n%s", path, raw)
		}
	}
}

func TestTopCodexCountsFoldsTheRestIntoOther(t *testing.T) {
	counts := map[string]int{"a": 5, "b": 4, "c": 3, "d": 2, "e": 1, "other": 10}
	got := topCodexCounts(counts, 2)
	want := map[string]int{"a": 5, "b": 4, "other": 16}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("topCodexCounts = %v, want %v", got, want)
	}
	if same := topCodexCounts(counts, 9); !reflect.DeepEqual(same, counts) {
		t.Fatalf("nothing to fold, got %v", same)
	}
}

func TestCodexRefusalNamesACode(t *testing.T) {
	for answer, want := range map[string]bool{
		`{"detail":"Bad Request"}`:                                                            false,
		`{"detail":"The 'x' model is not supported"}`:                                         false,
		`{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`: false,
		`{"error":{"code":""}}`:                                                               false,
		`{"error":"invalid_prompt"}`:                                                          false,
		`plain text failure`:                                                                  false,
		`<html>400 Bad Request</html>`:                                                        false,
		``:                                                                                    false,
		`{"error":{"code":"invalid_encrypted_content"}}`:                                      true,
		`{"error":{"code":"invalid_prompt","message":"x"}}`:                                   true,
		`{"code":"unsupported_value"}`:                                                        true,
		`{"detail":{"code":"model_not_supported","message":"x"}}`:                             true,
	} {
		if got := codexRefusalNamesACode([]byte(answer)); got != want {
			t.Errorf("codexRefusalNamesACode(%q) = %v, want %v", answer, got, want)
		}
	}
}

func TestCodexBodyEditsNilReceiverIsANoOp(t *testing.T) {
	var edits *codexBodyEdits
	edits.noteImageBridgeTool()
	edits.noteToolChoiceForced()
	edits.noteEffortDropped()
	edits.noteHistory(1, 2, 3, 4, true)
	edits.noteForeignReasoning(5)
	if firstCodexEdits(nil) != nil || firstCodexEdits([]*codexBodyEdits{}) != nil {
		t.Fatal("no tally given means none")
	}
	if codexBodyEditsFromContext(nil) != nil { //nolint:staticcheck // a nil context must not panic.
		t.Fatal("a nil context carries no tally")
	}
}

func TestStripCodexHistoryTalliesWhatItChanges(t *testing.T) {
	png := strings.Repeat("Q", 300)
	body := []byte(`{"store":false,"input":[` +
		`{"type":"message","id":"msg_u1","role":"user","content":[{"type":"input_text","text":"one"}]},` +
		`{"type":"message","id":"msg_a1","role":"assistant","content":[{"type":"output_text","text":"here ![x](data:image/png;base64,` + png + `)"}]},` +
		`{"type":"function_call_output","id":"fco_1","call_id":"c1"},` +
		`{"type":"image_generation_call","id":"ig_1","status":"completed","result":"` + png + `"},` +
		`{"type":"message","id":"msg_u2","role":"user","content":[{"type":"input_text","text":"two"}]}]}`)

	edits := &codexBodyEdits{}
	out := stripCodexHistoryDataURLImagesForStore(body, true, edits)
	want := codexBodyEdits{IDsStripped: 3, ShellsDropped: 2, DataURLsReplaced: 1, ImageResultsDropped: 1, ImageReattached: true}
	if *edits != want {
		t.Fatalf("edits = %+v, want %+v\nbody %s", *edits, want, out)
	}
	if got := gjson.GetBytes(out, "input.#").Int(); got != 3 {
		t.Fatalf("input items = %d, want the two shells gone", got)
	}

	// The pass runs again on the translated body. Nothing is left to change, so
	// the tally does not move.
	stripCodexHistoryDataURLImagesForStore(out, true, edits)
	if *edits != want {
		t.Fatalf("a second pass over clean history changed the tally: %+v", *edits)
	}

	// Without ids to strip only the pixels are touched; without a tally the output is the same.
	plain := stripCodexHistoryDataURLImagesForStore(body, true)
	if string(plain) != string(out) {
		t.Fatal("passing a tally must not change what the pass produces")
	}
	only := &codexBodyEdits{}
	stripCodexHistoryDataURLImagesForStore(body, false, only)
	if *only != (codexBodyEdits{DataURLsReplaced: 1, ImageResultsDropped: 1, ImageReattached: true}) {
		t.Fatalf("store=true keeps ids and shells: %+v", *only)
	}

	// A string input is a different path through the function.
	stringInput := &codexBodyEdits{}
	stripCodexHistoryDataURLImagesForStore([]byte(`{"input":"look ![a](data:image/png;base64,`+png+`) and ![b](data:image/png;base64,`+png+`)"}`), false, stringInput)
	if *stringInput != (codexBodyEdits{DataURLsReplaced: 2}) {
		t.Fatalf("string input edits = %+v", *stringInput)
	}
}

func TestPrepareCodexResponsesBodyTalliesForeignReasoning(t *testing.T) {
	body := []byte(responsesWithInput(reasoningItem(openAICipherA), reasoningItem(claudeSignaure), reasoningItem(geminiSignaure)))
	edits := &codexBodyEdits{}
	out := prepareCodexResponsesBody(body, nil, "", true, edits)
	if edits.ForeignReasoningDropped != 2 {
		t.Fatalf("foreign reasoning dropped = %d, want 2", edits.ForeignReasoningDropped)
	}
	if got := replayedCiphertexts(out); len(got) != 1 || got[0] != openAICipherA {
		t.Fatalf("ciphertexts left = %v", got)
	}
	// An API-key upstream is never touched, so nothing is tallied.
	untouched := &codexBodyEdits{}
	prepareCodexResponsesBody(body, nil, "", false, untouched)
	if *untouched != (codexBodyEdits{}) {
		t.Fatalf("API-key upstream tallied %+v", *untouched)
	}
}

func TestImageBridgeTalliesItsEdits(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex"}
	explicit := []byte(`{"model":"gpt-5.4","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"[$imagegen] draw a cat"}]}]}`)
	edits := &codexBodyEdits{}
	out := maybeEnsureCodexImageGenerationTool(explicit, auth, "gpt-5.4", nil, edits)
	if want := (codexBodyEdits{ImageBridgeTool: true, ToolChoiceForced: true, EffortDropped: true}); *edits != want {
		t.Fatalf("edits = %+v, want %+v\nbody %s", *edits, want, out)
	}

	// An ordinary turn gets the tool and keeps its effort; nothing is forced.
	ordinary := []byte(`{"model":"gpt-5.4","tools":[{"type":"function","name":"shell"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor this"}]}]}`)
	edits = &codexBodyEdits{}
	maybeEnsureCodexImageGenerationTool(ordinary, auth, "gpt-5.4", nil, edits)
	if want := (codexBodyEdits{ImageBridgeTool: true}); *edits != want {
		t.Fatalf("edits = %+v, want %+v", *edits, want)
	}

	// A client that brings the tool itself is not changed, and the tally says so.
	own := []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"refactor this"}]}]}`)
	edits = &codexBodyEdits{}
	maybeEnsureCodexImageGenerationTool(own, auth, "gpt-5.4", nil, edits)
	if *edits != (codexBodyEdits{}) {
		t.Fatalf("edits = %+v, want none", *edits)
	}

	// The same call without a tally behaves the same.
	if string(maybeEnsureCodexImageGenerationTool(explicit, auth, "gpt-5.4", nil)) != string(out) {
		t.Fatal("passing a tally must not change what the bridge produces")
	}
}

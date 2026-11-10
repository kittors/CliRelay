package chat_completions

import (
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var updateRequestGolden = flag.Bool("update", false, "rewrite testdata/request/*.json from the current translation")

// requestGoldenCases reach every branch of ConvertOpenAIRequestToCodex. The
// translated body is what the Codex upstream sees, so a change meant to be
// internal, such as how the input array is assembled, has to keep these bytes
// exactly. The files were first written by the implementation that appended
// each content part to its message with sjson.
var requestGoldenCases = []struct {
	name   string
	stream bool
	input  string
}{
	{name: "text_roles", stream: true, input: `{"model":"gpt-5.4","messages":[{"role":"system","content":"You are terse."},{"role":"developer","content":"Use tools."},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":""},{"content":"no role"},{"role":"user"}],"stream":true}`},
	{name: "multimodal_parts", stream: true, input: `{"messages":[{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA"}},{"type":"text","text":"and this"},{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"high"}},{"type":"text","text":"what changed?"},{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0xLjQ=","filename":"a.pdf"}},{"type":"file","file":{"file_data":"data:text/plain;base64,aGk="}},{"type":"file","file":{"filename":"empty.txt"}},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]},{"role":"assistant","content":[{"type":"text","text":"I see"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},{"role":"user","content":[{"type":"image_url","image_url":{}}]},{"role":"user","content":[]}]}`},
	{name: "escaping", stream: true, input: `{"messages":[{"role":"user","content":[{"type":"text","text":"say \"hi\" \\ <b>&amp;</b>\nline2 é 中文 \u2028"},{"type":"image_url","image_url":{"url":"https:\/\/example.com\/x.png?a=1&b=2"}}]},{"role":"assistant","content":"done é"},{"role":"we\"ird","content":"odd role"}]}`},
	{name: "tool_calls_and_results", stream: true, input: `{"messages":[{"role":"user","content":"run it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_2","type":"function","function":{"name":"mcp__very_long_server_name_that_goes_on_and_on_and_on__lookup_record_by_identifier","arguments":"{}"}},{"id":"call_3","type":"custom","custom":{"name":"x","input":"y"}},{"id":"call_4","type":"function","function":{"name":"this_is_a_really_long_function_name_without_the_mcp_prefix_that_exceeds_the_limit","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"},{"role":"tool","tool_call_id":"call_2","content":[{"type":"text","text":"found"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]},"strict":true}},{"type":"function","function":{"name":"mcp__very_long_server_name_that_goes_on_and_on_and_on__lookup_record_by_identifier","parameters":{"type":"object"}}},{"type":"web_search"}],"tool_choice":{"type":"function","function":{"name":"mcp__very_long_server_name_that_goes_on_and_on_and_on__lookup_record_by_identifier"}},"reasoning_effort":"high","parallel_tool_calls":false}`},
	{name: "json_object_without_mention", stream: true, input: `{"messages":[{"role":"user","content":"weather?"}],"response_format":{"type":"json_object"},"text":{"verbosity":"low"}}`},
	{name: "json_object_with_mention", stream: true, input: `{"messages":[{"role":"user","content":[{"type":"text","text":"Answer in JSON please"}]}],"response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"auto"}`},
	{name: "json_schema", stream: true, input: `{"messages":[{"role":"system","content":"Extract."},{"role":"user","content":"Paris is in France"}],"response_format":{"type":"json_schema","json_schema":{"name":"city","description":"A city","schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false},"strict":true}},"text":{"verbosity":"high"},"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"f"}]}}`},
	{name: "text_format_not_streaming", stream: false, input: `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"},"tool_choice":"none","stream":false}`},
	{name: "tool_choice_function_without_name", stream: true, input: `{"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"function"}}`},
	{name: "tool_choice_without_type", stream: true, input: `{"messages":[{"role":"user","content":"x"}],"tool_choice":{"mode":"auto"},"tools":[]}`},
	{name: "messages_not_an_array", stream: true, input: `{"messages":"hello","reasoning_effort":"low"}`},
	{name: "empty_request", stream: true, input: `{}`},
	{name: "duplicate_keys", stream: true, input: `{"messages":[{"role":"user","content":"first","content":"second"}],"messages":[{"role":"user","content":"ignored"}]}`},
	{name: "screenshot_history", stream: true, input: string(screenshotHistoryRequest(2, 96))},
}

func TestConvertOpenAIRequestToCodexMatchesGolden(t *testing.T) {
	for _, tc := range requestGoldenCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConvertOpenAIRequestToCodex("gpt-5.4", []byte(tc.input), tc.stream)
			path := filepath.Join("testdata", "request", tc.name+".json")
			if *updateRequestGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden: %v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Fatalf("translation changed\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// BenchmarkConvertOpenAIRequestToCodexScreenshotHistory translates the shape
// that dominates relay traffic: a long agent history whose user turns each
// re-send a screenshot between text parts, about 10 MB in all.
func BenchmarkConvertOpenAIRequestToCodexScreenshotHistory(b *testing.B) {
	input := screenshotHistoryRequest(24, 400<<10)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for b.Loop() {
		ConvertOpenAIRequestToCodex("gpt-5.4", input, true)
	}
}

// screenshotHistoryRequest builds an agent conversation of turns rounds. Each
// user turn carries text, a screenshot of imageSize base64 bytes and two more
// text parts; each assistant turn calls a tool whose result follows. The tools
// come after the messages, as the OpenAI SDKs send them.
func screenshotHistoryRequest(turns, imageSize int) []byte {
	image := strings.Repeat("iVBORw0KGgoAAAANSUhEUgAA", imageSize/24+1)[:imageSize]
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"system","content":"You drive a browser."}`)
	for i := range turns {
		n := strconv.Itoa(i)
		b.WriteString(`,{"role":"user","content":[{"type":"text","text":"Step ` + n + `: here is the screen."},{"type":"image_url","image_url":{"url":"data:image/png;base64,`)
		b.WriteString(image)
		b.WriteString(`"}},{"type":"text","text":"The page finished loading."},{"type":"text","text":"What next?"}]}`)
		b.WriteString(`,{"role":"assistant","content":"Clicking the button.","tool_calls":[{"id":"call_` + n + `","type":"function","function":{"name":"click","arguments":"{\"x\":10,\"y\":20}"}}]}`)
		b.WriteString(`,{"role":"tool","tool_call_id":"call_` + n + `","content":"clicked"}`)
	}
	b.WriteString(`],"tools":[{"type":"function","function":{"name":"click","parameters":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}}}}]}`)
	return []byte(b.String())
}

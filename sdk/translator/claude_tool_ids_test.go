package translator

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// anthropicToolIDPattern is the pattern the Messages API documents for
// tool_use.id and tool_result.tool_use_id.
var anthropicToolIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func TestClaudeToolIDRoundTrip(t *testing.T) {
	for _, id := range []string{
		"functions.Bash:0",
		"functions.mcp__playwright__browser_navigate:12",
		"mcp/server.tool:1",
		"read.file-1730000000000000000-3",
		"调用:1",
		`quote"and\backslash`,
		"<tag>&amp;",
		"with space",
		// In the alphabet, but carrying the prefix: encoded so it decodes back.
		"toolu_enc_",
		"toolu_enc_ZnVuY3Rpb25zLkJhc2g6MA",
	} {
		encoded := encodeClaudeToolID(id)
		if !strings.HasPrefix(encoded, claudeToolIDPrefix) || !anthropicToolIDPattern.MatchString(encoded) {
			t.Errorf("encode(%q) = %q, not a prefixed id in the Anthropic alphabet", id, encoded)
		}
		if decoded, ok := decodeClaudeToolID(encoded); !ok || decoded != id {
			t.Errorf("decode(encode(%q)) = %q, %v", id, decoded, ok)
		}
	}
}

func TestCompliantClaudeToolIDsAreUnchanged(t *testing.T) {
	for _, id := range []string{
		"toolu_01A09q90qw90lq917835lq9",
		"call_abc-123",
		"tool_0",
		"read_file-1730000000000000000-3",
		"toolu_enc",
		"toolu-enc_x",
		"",
	} {
		if got := encodeClaudeToolID(id); got != id {
			t.Errorf("encode(%q) = %q, want it unchanged", id, got)
		}
		if got, ok := decodeClaudeToolID(id); ok {
			t.Errorf("decode(%q) = %q, want it left alone", id, got)
		}
	}
}

func TestMalformedEncodedClaudeToolIDsAreLeftAlone(t *testing.T) {
	for _, id := range []string{
		"toolu_enc_",      // nothing after the prefix
		"toolu_enc_!!!",   // not base64url
		"toolu_enc_YS+i",  // standard base64, not base64url
		"toolu_enc_YS4=",  // padded
		"toolu_enc_YS5",   // "a." with stray trailing bits: not what encode writes
		"toolu_enc_YS\n4", // "a." with a newline inside, which base64 skips
		"toolu_enc_Y",     // truncated
		"toolu_enc_YQ",    // "a", which never needed encoding
		"toolu_enc_gA",    // a lone 0x80 byte, not UTF-8
	} {
		if got, ok := decodeClaudeToolID(id); ok {
			t.Errorf("decode(%q) = %q, want it rejected", id, got)
		}
	}
}

func FuzzClaudeToolIDCodec(f *testing.F) {
	for _, seed := range []string{"", "functions.Bash:0", "toolu_01A", "toolu_enc_", "toolu_enc_YS4", "toolu_enc_gA", "a/b:c.d", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		encoded := encodeClaudeToolID(id)
		if id != "" && !anthropicToolIDPattern.MatchString(encoded) {
			t.Fatalf("encode(%q) = %q, outside the Anthropic alphabet", id, encoded)
		}
		// Ids come out of JSON, so they are UTF-8; those round-trip.
		if utf8.ValidString(id) {
			back := encoded
			if decoded, ok := decodeClaudeToolID(encoded); ok {
				back = decoded
			}
			if back != id {
				t.Fatalf("%q -> %q -> %q", id, encoded, back)
			}
		}
		// Whatever decode accepts, encode gives back.
		if decoded, ok := decodeClaudeToolID(id); ok && encodeClaudeToolID(decoded) != id {
			t.Fatalf("decode(%q) = %q, which encodes to %q", id, decoded, encodeClaudeToolID(decoded))
		}
	})
}

func TestDecodeClaudeRequestToolIDs(t *testing.T) {
	const kimi = "functions.Bash:0"
	encoded := encodeClaudeToolID(kimi)
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"run ` + encoded + `"},` +
		`{"role":"assistant","content":[{"type":"text","text":"calling ` + encoded + `"},{"type":"tool_use","id":"` + encoded + `","name":"Bash","input":{"note":"` + encoded + `"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + encoded + `","content":"ok"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01A","name":"Read","input":{}},{"type":"tool_use","id":"toolu_enc_!!","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"a"},{"type":"tool_result","tool_use_id":"toolu_enc_!!","content":"b"}]}` +
		`]}`

	got := decodeClaudeRequestToolIDs([]byte(body))
	// Only the two id fields change; the same text in prose, tool input and
	// malformed or plain ids stays.
	want := strings.Replace(body, `"id":"`+encoded+`"`, `"id":"`+kimi+`"`, 1)
	want = strings.Replace(want, `"tool_use_id":"`+encoded+`"`, `"tool_use_id":"`+kimi+`"`, 1)
	if string(got) != want {
		t.Fatalf("decoded request:\n got %s\nwant %s", got, want)
	}

	// A decoded id that needs JSON escaping is written back as valid JSON,
	// and otherwise as the upstream wrote it.
	quoted := `say "hi"\now`
	escaped := decodeClaudeRequestToolIDs([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + encodeClaudeToolID(quoted) + `","content":"x"}]}]}`))
	if !gjson.ValidBytes(escaped) || gjson.GetBytes(escaped, "messages.0.content.0.tool_use_id").String() != quoted {
		t.Fatalf("escaped id: %s", escaped)
	}
	markup := decodeClaudeRequestToolIDs([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + encodeClaudeToolID("<a>&b") + `","content":"x"}]}]}`))
	if !bytes.Contains(markup, []byte(`"tool_use_id":"<a>&b"`)) {
		t.Fatalf("markup id: %s", markup)
	}
}

func TestDecodeClaudeRequestToolIDsLeavesOtherBodiesAlone(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"no prefix anywhere"}]}]}`,
		`{"messages":"toolu_enc_ZnVuY3Rpb25zLkJhc2g6MA"}`,
		`{"messages":[{"role":"user","content":"toolu_enc_ZnVuY3Rpb25zLkJhc2g6MA"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":["toolu_enc_ZnVuY3Rpb25zLkJhc2g6MA"]}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_enc_ZnVuY3Rpb25zLkJhc2g6MA`,
		`[]`,
		``,
	} {
		in := []byte(body)
		if got := decodeClaudeRequestToolIDs(in); !bytes.Equal(got, in) {
			t.Errorf("body changed:\n in %s\nout %s", in, got)
		}
	}
}

func TestEncodeClaudeResponseToolIDs(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[` +
		`{"type":"text","text":"calling functions.Bash:0"},` +
		`{"type":"tool_use","id":"functions.Bash:0","name":"Bash","input":{"command":"ls"}},` +
		`{"type":"tool_use","id":"toolu_01A","name":"Read","input":{}},` +
		// Server tool ids follow a stricter pattern the encoding does not
		// meet; they are Anthropic's and stay as they are.
		`{"type":"server_tool_use","id":"srvtoolu_01B.x","name":"web_search","input":{}}` +
		`],"stop_reason":"tool_use"}`
	got := EncodeClaudeResponseToolIDs([]byte(body))
	want := strings.Replace(body, `"id":"functions.Bash:0"`, `"id":"`+encodeClaudeToolID("functions.Bash:0")+`"`, 1)
	if string(got) != want {
		t.Fatalf("encoded response:\n got %s\nwant %s", got, want)
	}

	plain := []byte(`{"content":[{"type":"tool_use","id":"toolu_01A","name":"Read","input":{}}]}`)
	if got := EncodeClaudeResponseToolIDs(plain); &got[0] != &plain[0] {
		t.Fatal("a response with compliant ids was copied")
	}
	// A body that is not JSON is passed on as it came.
	broken := []byte(`{"content":[{"type":"tool_use","id":"functions.Bash:0","name":"Bash","input":{}}`)
	if got := EncodeClaudeResponseToolIDs(broken); !bytes.Equal(got, broken) {
		t.Fatalf("broken body changed: %s", got)
	}
}

func TestEncodeClaudeStreamToolIDs(t *testing.T) {
	start := func(id string) string {
		return `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}}`
	}
	encoded := encodeClaudeToolID("functions.Bash:0")

	// A translator's output: whole events, several to a chunk.
	chunk := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: " + start("functions.Bash:0") + "\n\n"
	want := "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: " + start(encoded) + "\n\n"
	if got := EncodeClaudeStreamToolIDs([]byte(chunk)); string(got) != want {
		t.Fatalf("translated chunk:\n got %q\nwant %q", got, want)
	}

	// The Claude executor's pass-through: one line per chunk, no space after
	// the colon, CRLF line ends.
	line := "data:" + start("functions.Bash:0") + "\r\n"
	if got := EncodeClaudeStreamToolIDs([]byte(line)); string(got) != "data:"+start(encoded)+"\r\n" {
		t.Fatalf("pass-through line: %q", got)
	}

	// Events that are not a tool_use start are left as they are, even when
	// they mention one.
	for _, unchanged := range []string{
		"data: " + start("toolu_01A") + "\n\n",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"type\":\"tool_use\",\"id\":\"functions.Bash:0\"}"}}` + "\n\n",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"srvtoolu_01B.x","name":"web_search","input":{}}}` + "\n\n" +
			"data: " + start("toolu_01A") + "\n\n",
		"event: ping\n\n",
		// A line cut off inside the id, its newline still to come.
		"data: " + start("functions.Bash:0")[:95],
		"data: " + start("functions.Bash:0"),
		// A line that is not JSON.
		"data: " + start("functions.Bash:0")[:105] + "\n\n",
	} {
		in := []byte(unchanged)
		if got := EncodeClaudeStreamToolIDs(in); !bytes.Equal(got, in) {
			t.Errorf("chunk changed:\n in %q\nout %q", in, got)
		}
	}
}

func TestRegistryDecodesClaudeToolIDsOnlyWhenLeavingClaudeFormat(t *testing.T) {
	r := NewRegistry()
	var seen []byte
	capture := func(_ string, raw []byte, _ bool) []byte {
		seen = raw
		return raw
	}
	r.Register(FormatClaude, FormatOpenAI, capture, ResponseTransform{})
	r.Register(FormatOpenAI, FormatClaude, capture, ResponseTransform{})
	r.Register(FormatClaude, FormatClaude, capture, ResponseTransform{})
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + encodeClaudeToolID("functions.Bash:0") + `","content":"ok"}]}]}`)
	original := bytes.Clone(body)

	r.TranslateRequest(FormatClaude, FormatOpenAI, "m", body, false)
	if got := gjson.GetBytes(seen, "messages.0.content.0.tool_use_id").String(); got != "functions.Bash:0" {
		t.Fatalf("Claude to OpenAI translator saw tool_use_id %q", got)
	}
	if !bytes.Equal(body, original) {
		t.Fatal("the caller's request was modified")
	}

	// A request that stays in Claude format keeps the encoded id: Claude
	// upstreams check the same pattern Claude clients do.
	seen = nil
	r.TranslateRequest(FormatClaude, FormatClaude, "m", body, false)
	if !bytes.Equal(seen, original) {
		t.Fatalf("Claude to Claude translator saw %s", seen)
	}
	// Only a Claude client's ids were encoded.
	seen = nil
	r.TranslateRequest(FormatOpenAI, FormatClaude, "m", body, false)
	if !bytes.Equal(seen, original) {
		t.Fatalf("OpenAI to Claude translator saw %s", seen)
	}
}

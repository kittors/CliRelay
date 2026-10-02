package common

import (
	"github.com/tidwall/gjson"
	"strings"
	"testing"
)

func TestParseToolResultPreservesStructuredData(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `[{"foo":"bar"}]`, `{"text":"keep","metadata":{"id":7}}`, `[{"text":"keep","metadata":true}]`, `{"result":false}`} {
		t.Run(raw, func(t *testing.T) {
			r := ParseToolResult(gjson.Parse(raw))
			if r.Raw.Raw != raw || len(r.Parts) != 0 {
				t.Fatalf("structured result changed: %#v", r)
			}
		})
	}
}

func TestParseToolResultMixedUnknownKeepsImageSeparate(t *testing.T) {
	raw := `[{"type":"text","text":"before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMAGEBASE64"}},{"type":"future","value":"keep"}]`
	r := ParseToolResult(gjson.Parse(raw))
	if !r.HasImage() || len(r.Parts) != 3 {
		t.Fatalf("parts lost: %#v", r)
	}
	out := gjson.Parse(ToChatContent(r))
	if out.Get("1.image_url.url").String() != "data:image/png;base64,IMAGEBASE64" {
		t.Fatalf("image not encoded as image: %s", out.Raw)
	}
	if !strings.Contains(out.Get("2.text").String(), `"value":"keep"`) {
		t.Fatalf("unknown data lost: %s", out.Raw)
	}
	for _, p := range out.Array() {
		if strings.Contains(p.Get("text").String(), "IMAGEBASE64") {
			t.Fatalf("base64 leaked to text: %s", out.Raw)
		}
	}
}

func TestParseGeminiFunctionResponseParts(t *testing.T) {
	cases := []struct{ name, part, wantURL string }{
		{"inline camel", `{"inlineData":{"mimeType":"image/png","data":"AAAA"}}`, "data:image/png;base64,AAAA"},
		{"inline snake", `{"inline_data":{"mime_type":"image/png","data":"AAAA"}}`, "data:image/png;base64,AAAA"},
		{"file camel", `{"fileData":{"mimeType":"image/jpeg","fileUri":"https://example.com/a.jpg"}}`, "https://example.com/a.jpg"},
		{"file snake", `{"file_data":{"mime_type":"image/jpeg","file_uri":"https://example.com/a.jpg"}}`, "https://example.com/a.jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := ParseGeminiFunctionResponse(gjson.Parse(`{"response":{"result":"done"},"parts":[{"text":"extra"},` + tc.part + `]}`))
			out := gjson.Parse(ToResponsesOutput(r))
			if out.Get("0.text").String() != "done" || out.Get("1.text").String() != "extra" || out.Get("2.image_url").String() != tc.wantURL {
				t.Fatalf("output=%s", out.Raw)
			}
		})
	}
}

func TestGeminiFunctionResponseMixedTextRoundTrip(t *testing.T) {
	r := ParseToolResult(gjson.Parse(`[{"type":"text","text":"A"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"B"}]`))
	fr := SetGeminiFunctionResponse(`{}`, r)
	back := ParseGeminiFunctionResponse(gjson.Parse(fr))
	if back.Text() != "A\nB" || !back.HasImage() {
		t.Fatalf("round trip lost parts: %s %#v", fr, back)
	}
}

func TestGeminiFunctionResponseStructuredResultStaysText(t *testing.T) {
	r := ParseGeminiFunctionResponse(gjson.Parse(`{"response":{"result":{"text":"keep","metadata":true}}}`))
	for _, out := range []string{ToChatContent(r), ToClaudeContent(r), ToResponsesOutput(r)} {
		got := gjson.Parse(out)
		if got.Type != gjson.String || got.String() != `{"text":"keep","metadata":true}` {
			t.Fatalf("structured result changed: %s", out)
		}
	}
}

package translator

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/translator"
	"github.com/tidwall/gjson"
)

// Gemini, Gemini CLI and Antigravity replies reach a Responses client through
// one converter, each unwrapped its own way, so every one of them must end a
// reply that was cut short with response.incomplete, streaming and not. The
// stop reason matrix has no Responses client: the Claude and Chat Completions
// converters to Responses still end every reply with response.completed.
func TestGeminiFinishReasonsReachResponsesClients(t *testing.T) {
	ctx := context.Background()
	request := []byte(`{"model":"m","input":"hi"}`)
	streamRequest := []byte(`{"model":"m","stream":true,"input":"hi"}`)
	cases := []struct {
		reason string
		status string
		// incompleteReason is incomplete_details.reason; "" means it is left out.
		incompleteReason string
	}{
		{"STOP", "completed", ""},
		{"MAX_TOKENS", "incomplete", "max_output_tokens"},
		{"SAFETY", "incomplete", "content_filter"},
		{"RECITATION", "incomplete", "content_filter"},
		{"MALFORMED_FUNCTION_CALL", "incomplete", ""},
	}
	check := func(t *testing.T, resp gjson.Result, status, incompleteReason string) {
		t.Helper()
		if got := resp.Get("status").String(); got != status {
			t.Fatalf("status = %q, want %q: %s", got, status, resp.Raw)
		}
		if got := resp.Get("output.0.content.0.text").String(); got != "Hello" {
			t.Fatalf("output text = %q, want the text produced before the turn ended", got)
		}
		details := resp.Get("incomplete_details")
		if status != "incomplete" {
			if details.Exists() && details.Type != gjson.Null {
				t.Fatalf("incomplete_details = %s on a %s response", details.Raw, status)
			}
			return
		}
		reason := details.Get("reason")
		if !details.IsObject() || reason.String() != incompleteReason || (incompleteReason == "" && reason.Exists()) {
			t.Fatalf("incomplete_details = %s, want reason %q", details.Raw, incompleteReason)
		}
	}
	paths := 0
	for _, upstream := range stopUpstreams {
		if upstream.names != "gemini" {
			continue
		}
		if !translator.NeedConvert("openai-response", upstream.format) {
			t.Fatalf("no %s -> openai-response translator", upstream.format)
		}
		paths++
		for _, tc := range cases {
			name := upstream.format + "->openai-response/" + tc.reason
			t.Run(name+"/stream", func(t *testing.T) {
				var param any
				var outputs []string
				for _, chunk := range upstream.stream(tc.reason) {
					outputs = append(outputs, translator.Response(upstream.format, "openai-response", ctx, "m", streamRequest, streamRequest, []byte(chunk), &param)...)
				}
				payloads := sseData(outputs)
				terminals := 0
				for _, payload := range payloads {
					switch payload.Get("type").String() {
					case "response.completed", "response.incomplete", "response.failed":
						terminals++
					}
				}
				want := "response." + tc.status
				if len(payloads) == 0 || terminals != 1 || payloads[len(payloads)-1].Get("type").String() != want {
					t.Fatalf("want one %s at the end, got %d terminal events: %q", want, terminals, outputs)
				}
				check(t, payloads[len(payloads)-1].Get("response"), tc.status, tc.incompleteReason)
			})
			t.Run(name+"/non-stream", func(t *testing.T) {
				var param any
				out := translator.ResponseNonStream(upstream.format, "openai-response", ctx, "m", request, request, []byte(upstream.nonStream(tc.reason)), &param)
				check(t, gjson.Parse(out), tc.status, tc.incompleteReason)
			})
		}
	}
	if paths != 3 {
		t.Fatalf("%d Gemini-family upstreams exercised, want gemini, gemini-cli and antigravity", paths)
	}
}

package contentmoderation

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func replyFilterOf(t *testing.T, format sdktranslator.Format, action string, rules ...WordRule) (*wordReply, *WordGuardSnapshot) {
	t.Helper()
	snapshot := newWordGuardSnapshot(WordVerdict{Action: WordVerdictAllow}, nil, "")
	filter := newWordReply(replyContexts(t, action, rules...), format, snapshot)
	if filter == nil {
		t.Fatal("no reply filter")
	}
	return filter.(*wordReply), snapshot
}

// reply filters a whole reply and returns the response.
func reply(t *testing.T, format sdktranslator.Format, body string, rules ...WordRule) cliproxyexecutor.Response {
	t.Helper()
	filter, _ := replyFilterOf(t, format, WordActionReplace, rules...)
	resp := filter.FilterResponse(cliproxyexecutor.Response{
		Payload: []byte(body),
		Headers: http.Header{"Content-Length": {"123"}, "Content-Type": {"application/json"}},
	})
	if !gjson.ValidBytes(resp.Payload) {
		t.Fatalf("the filtered reply is not JSON: %s", resp.Payload)
	}
	return resp
}

func TestReplyFilterClaudeMessage(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[` +
		`{"type":"thinking","thinking":"the secret plan","signature":"secret-sig=="},` +
		`{"type":"text","text":"Here is the secret é answer"},` +
		`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"secret"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":4}}`
	want := strings.Replace(body, `Here is the secret é answer`, `Here is the *** é answer`, 1)
	resp := reply(t, sdktranslator.FormatClaude, body, kw("secret"))
	if string(resp.Payload) != want {
		t.Fatalf("got\n%s\nwant\n%s", resp.Payload, want)
	}
	if resp.Headers.Get("Content-Length") != "" || resp.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", resp.Headers)
	}
}

func TestReplyFilterOpenAIChatCompletion(t *testing.T) {
	body := `{"id":"chatcmpl-1","object":"chat.completion","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"a secret reply","reasoning_content":"secret thoughts","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"q\":\"secret\"}"}}]},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":[{"type":"text","text":"another secret"}]},"finish_reason":"stop"}],` +
		`"usage":{"total_tokens":9}}`
	want := strings.NewReplacer(`"a secret reply"`, `"a *** reply"`, `"another secret"`, `"another ***"`).Replace(body)
	resp := reply(t, sdktranslator.FormatOpenAI, body, kw("secret"))
	if string(resp.Payload) != want {
		t.Fatalf("got\n%s\nwant\n%s", resp.Payload, want)
	}
}

func TestReplyFilterOpenAIResponses(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","output":[` +
		`{"id":"rs_1","type":"reasoning","encrypted_content":"secret==","summary":[{"type":"summary_text","text":"secret summary"}]},` +
		`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"The secret is out","annotations":[]}]}],` +
		`"usage":{"total_tokens":2}}`
	want := strings.Replace(body, `The secret is out`, `The *** is out`, 1)
	resp := reply(t, sdktranslator.FormatOpenAIResponse, body, kw("secret"))
	if string(resp.Payload) != want {
		t.Fatalf("got\n%s\nwant\n%s", resp.Payload, want)
	}
}

func TestReplyFilterGemini(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"text":"thinking about the secret","thought":true},` +
		`{"text":"The secret answer"},` +
		`{"functionCall":{"name":"f","args":{"q":"secret"}}}]},"finishReason":"STOP","index":0}],` +
		`"usageMetadata":{"totalTokenCount":5}}`
	want := strings.Replace(body, `The secret answer`, `The *** answer`, 1)
	resp := reply(t, sdktranslator.FormatGemini, body, kw("secret"))
	if string(resp.Payload) != want {
		t.Fatalf("got\n%s\nwant\n%s", resp.Payload, want)
	}
	wrapped := `{"response":` + body + `,"traceId":"t"}`
	wantWrapped := `{"response":` + want + `,"traceId":"t"}`
	if got := reply(t, sdktranslator.FormatGeminiCLI, wrapped, kw("secret")); string(got.Payload) != wantWrapped {
		t.Fatalf("cli: got\n%s\nwant\n%s", got.Payload, wantWrapped)
	}
}

func TestReplyFilterLeavesAnUnchangedReplyAlone(t *testing.T) {
	filter, snapshot := replyFilterOf(t, sdktranslator.FormatClaude, WordActionReplace, kw("secret"))
	body := []byte(`{"type":"message","content":[{"type":"text","text":"nothing to see"}]}`)
	headers := http.Header{"Content-Length": {"70"}}
	resp := filter.FilterResponse(cliproxyexecutor.Response{Payload: body, Headers: headers})
	if &resp.Payload[0] != &body[0] || resp.Headers.Get("Content-Length") != "70" {
		t.Fatalf("an unchanged reply must be returned as it is: %s %v", resp.Payload, resp.Headers)
	}
	if snapshot.Notable() {
		t.Fatal("nothing happened, yet the snapshot is worth logging")
	}
}

func TestReplyFilterIgnoresAReplyItCannotRead(t *testing.T) {
	filter, _ := replyFilterOf(t, sdktranslator.FormatClaude, WordActionReplace, kw("secret"))
	for _, body := range []string{"", "not json at all secret", `{"type":"error","error":{"message":"a secret"}}`} {
		resp := filter.FilterResponse(cliproxyexecutor.Response{Payload: []byte(body)})
		if string(resp.Payload) != body {
			t.Fatalf("%q was changed to %q", body, resp.Payload)
		}
	}
}

func gzipped(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReplyFilterUnpacksAGzipBody(t *testing.T) {
	filter, _ := replyFilterOf(t, sdktranslator.FormatClaude, WordActionReplace, kw("secret"))
	body := `{"type":"message","content":[{"type":"text","text":"the secret"}]}`
	resp := filter.FilterResponse(cliproxyexecutor.Response{
		Payload: gzipped(t, body),
		Headers: http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"60"}},
	})
	if want := `{"type":"message","content":[{"type":"text","text":"the ***"}]}`; string(resp.Payload) != want {
		t.Fatalf("got %q", resp.Payload)
	}
	if resp.Headers.Get("Content-Encoding") != "" || resp.Headers.Get("Content-Length") != "" {
		t.Fatalf("the body is plain now, but the headers say %v", resp.Headers)
	}

	// A compressed body with nothing to change goes on exactly as it came.
	clean := gzipped(t, `{"type":"message","content":[{"type":"text","text":"fine"}]}`)
	resp = filter.FilterResponse(cliproxyexecutor.Response{Payload: clean, Headers: http.Header{"Content-Encoding": {"gzip"}}})
	if !bytes.Equal(resp.Payload, clean) || resp.Headers.Get("Content-Encoding") != "gzip" {
		t.Fatal("an unchanged compressed reply was altered")
	}
}

func TestReplyFilterReportsWhatItCannotUnpack(t *testing.T) {
	filter, snapshot := replyFilterOf(t, sdktranslator.FormatClaude, WordActionReplace, kw("secret"))
	truncated := gzipped(t, strings.Repeat("secret ", 100))[:20]
	resp := filter.FilterResponse(cliproxyexecutor.Response{Payload: truncated, Headers: http.Header{"Content-Encoding": {"gzip"}}})
	if !bytes.Equal(resp.Payload, truncated) {
		t.Fatal("a body that cannot be unpacked was changed")
	}
	if view := snapshot.view.Reply; view == nil || view.Skipped != "compressed_body" {
		t.Fatalf("the skip was not reported: %+v", view)
	}

	// An encoding the filter does not know leaves nothing readable behind.
	filter, snapshot = replyFilterOf(t, sdktranslator.FormatClaude, WordActionReplace, kw("secret"))
	filter.FilterResponse(cliproxyexecutor.Response{Payload: []byte{0x28, 0xb5, 0x2f, 0xfd, 1, 2, 3}, Headers: http.Header{"Content-Encoding": {"zstd"}}})
	if view := snapshot.view.Reply; view == nil || view.Skipped != "unsupported_encoding" {
		t.Fatalf("the skip was not reported: %+v", view)
	}
}

func TestGunzipLimitedRefusesABodyBeyondTheLimit(t *testing.T) {
	body := gzipped(t, strings.Repeat("a", 5000))
	if _, err := gunzipLimited(body, 4999); err == nil {
		t.Fatal("a body larger than the limit was unpacked")
	}
	if plain, err := gunzipLimited(body, 5000); err != nil || len(plain) != 5000 {
		t.Fatalf("a body at the limit: %d, %v", len(plain), err)
	}
}

func TestNewWordReplyIsNilWithoutAContextThatAsked(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) { p.Action = WordActionReplace }, kw("x"))
	if newWordReply(buildWordContexts(nil, tenant, nil), sdktranslator.FormatClaude, nil) != nil {
		t.Fatal("a reply filter exists although no policy asked for one")
	}
}

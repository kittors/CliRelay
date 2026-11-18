package executor

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	incResponsesUser1 = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`
	incResponsesUser2 = `{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}`
)

// runResponsesTurn streams one Responses turn and returns the final event.
func runResponsesTurn(t *testing.T, exec cliproxyauth.ProviderExecutor, ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options, body string) gjson.Result {
	t.Helper()
	format := sdktranslator.FromString("openai-response")
	opts.SourceFormat, opts.Stream = format, true
	stream, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(body), Format: format}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var final gjson.Result
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		for _, line := range strings.Split(string(chunk.Payload), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
			if gjson.Get(line, "type").String() == "response.completed" {
				final = gjson.Parse(line)
			}
		}
	}
	if !final.Exists() {
		t.Fatal("stream ended without response.completed")
	}
	return final
}

func responsesBody(input ...string) string {
	return `{"model":"gpt-5.4","stream":true,"input":[` + strings.Join(input, ",") + `]}`
}

// A client on the Responses websocket sends increments; the relay expands them
// to the full input and, now, sends an increment on upstream too. Closing the
// client socket frees the upstream one.
func TestCodexIncrementalFollowsAClientWebsocketSession(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReasoning, incReplyText1}, []string{incReplyText2})
	exec, pool := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "client-ws-1"}}

	first := runResponsesTurn(t, exec, incCtx(), upstream.auth(), opts, responsesBody(incResponsesUser1))
	echoed := []string{incResponsesUser1}
	for _, item := range first.Get("response.output").Array() {
		echoed = append(echoed, item.Raw)
	}
	runResponsesTurn(t, exec, incCtx(), upstream.auth(), opts, responsesBody(append(echoed, incResponsesUser2)...))

	msgs := upstream.websocketMessages()
	if len(msgs) != 2 || gjson.Get(msgs[1].raw, "previous_response_id").String() != "resp_1" || gjson.Get(msgs[1].raw, "input").Raw != "["+incResponsesUser2+"]" {
		t.Fatalf("upstream messages = %+v, want the second turn as an increment", msgs)
	}

	auto := &CodexAutoExecutor{httpExec: exec, wsExec: NewCodexWebsocketsExecutor(exec.cfg)}
	auto.CloseExecutionSession("client-ws-1")
	if size := pool.size(); size.chains != 0 || size.conns != 0 {
		t.Fatalf("pool after the client closed its socket: %+v", size)
	}
	waitFor(t, func() bool { return upstream.readErrors.Load() == 1 }, "the upstream socket to close")
}

// An upstream request on a websocket that was not wanted any more (the client
// went away mid-reply) leaves events on their way; the connection goes, and the
// next turn starts over on a new one.
func TestCodexIncrementalDropsTheSocketOfAnAbandonedTurn(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1}, []string{incReplyText2}, []string{incReplyText3})
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	auth := upstream.auth()
	first := runResponsesTurn(t, exec, incCtx(), auth, cliproxyexecutor.Options{}, responsesBody(incResponsesUser1))
	echoed := []string{incResponsesUser1}
	for _, item := range first.Get("response.output").Array() {
		echoed = append(echoed, item.Raw)
	}

	upstream.stall.Store(true)
	ctx, cancel := context.WithCancel(incCtx())
	format := sdktranslator.FromString("openai-response")
	stream, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(responsesBody(append(echoed, incResponsesUser2)...)), Format: format}, cliproxyexecutor.Options{SourceFormat: format, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	<-stream.Chunks
	cancel()
	for range stream.Chunks {
	}
	waitFor(t, func() bool { return upstream.readErrors.Load() == 1 }, "the abandoned socket to close")

	runResponsesTurn(t, exec, incCtx(), auth, cliproxyexecutor.Options{}, responsesBody(append(echoed, incResponsesUser2)...))
	msgs := upstream.websocketMessages()
	last := msgs[len(msgs)-1]
	if last.conn != 2 || gjson.Get(last.raw, "previous_response_id").Exists() {
		t.Fatalf("turn after the abandoned one = %s on conn %d, want the whole input on a new connection", last.raw, last.conn)
	}
}

// At the backend's 60-minute limit the socket refuses the next turn; the turn
// goes again, in full, on a new socket.
func TestCodexIncrementalReconnectsAtTheConnectionLimit(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1}, []string{incReplyText2})
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	first := runResponsesTurn(t, exec, incCtx(), upstream.auth(), cliproxyexecutor.Options{}, responsesBody(incResponsesUser1))
	echoed := []string{incResponsesUser1}
	for _, item := range first.Get("response.output").Array() {
		echoed = append(echoed, item.Raw)
	}
	limit := `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Responses websocket connection limit reached (60 minutes). Create a new websocket connection to continue."}}`
	upstream.errorNext.Store(&limit)
	runResponsesTurn(t, exec, incCtx(), upstream.auth(), cliproxyexecutor.Options{}, responsesBody(append(echoed, incResponsesUser2)...))

	msgs := upstream.websocketMessages()
	if len(msgs) != 3 || msgs[2].conn != 2 || gjson.Get(msgs[2].raw, "previous_response_id").Exists() || len(gjson.Get(msgs[2].raw, "input").Array()) != 3 {
		t.Fatalf("upstream messages = %+v, want the refused turn again in full on a second connection", msgs)
	}
}

// The handshake is sent once per connection, so a turn the handshake would have
// described differently (here another model, which may need another Version
// header) does not go out on the old socket.
func TestCodexIncrementalOpensANewSocketForAnotherModel(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1}, []string{incReplyText2})
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "on"})
	first := runResponsesTurn(t, exec, incCtx(), upstream.auth(), cliproxyexecutor.Options{}, responsesBody(incResponsesUser1))
	echoed := []string{incResponsesUser1}
	for _, item := range first.Get("response.output").Array() {
		echoed = append(echoed, item.Raw)
	}
	format := sdktranslator.FromString("openai-response")
	body := strings.Replace(responsesBody(append(echoed, incResponsesUser2)...), `"gpt-5.4"`, `"gpt-5.5"`, 1)
	stream, err := exec.ExecuteStream(incCtx(), upstream.auth(), cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(body), Format: format}, cliproxyexecutor.Options{SourceFormat: format, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range stream.Chunks {
	}
	msgs := upstream.websocketMessages()
	if len(msgs) != 2 || msgs[1].conn != 2 || gjson.Get(msgs[1].raw, "previous_response_id").Exists() {
		t.Fatalf("upstream messages = %+v, want the other model's turn in full on a new socket", msgs)
	}
}

// The request log carries the comparison, so the hit rate can be read per key.
func TestCodexIncrementalRecordReachesTheRequestLog(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1})
	exec, _ := newIncExecutor(t, config.CodexUpstreamIncrementalConfig{Mode: "shadow"})
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ctx := context.WithValue(incCtx(), util.ContextKeyGin, ginCtx)
	runResponsesTurn(t, exec, ctx, upstream.auth(), cliproxyexecutor.Options{}, responsesBody(incResponsesUser1))

	detail := gjson.Parse(buildRequestDetailContent(ctx, false))
	record := detail.Get("codex_incremental")
	if record.Get("mode").String() != "shadow" || record.Get("result").String() != "miss" || record.Get("reason").String() != "no_previous_turn" || record.Get("transport").String() != "http" || record.Get("full_bytes").Int() == 0 {
		t.Fatalf("codex_incremental = %s", record.Raw)
	}
}

// The websocket executor (accounts with the websockets flag) continues with
// response.create plus previous_response_id. It used to rewrite that into
// response.append, which the backend answers with 400 "Expected a
// 'response.create' websocket message."
func TestCodexWebsocketsExecutorContinuesWithResponseCreate(t *testing.T) {
	upstream := newIncUpstream(t, []string{incReplyText1}, []string{incReplyText2})
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	t.Cleanup(func() { exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID) })
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "legacy-ws"}}
	runResponsesTurn(t, exec, incCtx(), upstream.auth(), opts, responsesBody(incResponsesUser1))
	runResponsesTurn(t, exec, incCtx(), upstream.auth(), opts, `{"model":"gpt-5.4","stream":true,"previous_response_id":"resp_1","input":[`+incResponsesUser2+`]}`)

	msgs := upstream.websocketMessages()
	if len(msgs) != 2 || gjson.Get(msgs[1].raw, "type").String() != "response.create" || gjson.Get(msgs[1].raw, "previous_response_id").String() != "resp_1" {
		t.Fatalf("upstream messages = %+v, want a response.create continuing resp_1", msgs)
	}
}

func waitFor(t *testing.T, done func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

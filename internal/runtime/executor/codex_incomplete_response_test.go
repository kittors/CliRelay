package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const codexIncompleteMessageItem = `{"type":"message","id":"msg_1","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"Hello","annotations":[]}]}`

// codexMaxOutputTokensSSE is a Codex reply cut short by max_output_tokens. The
// final event carries no output: over HTTP the executor collects the message
// from response.output_item.done, as it does for response.completed.
const codexMaxOutputTokensSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"gpt-5.4\",\"output\":[]}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"status\":\"in_progress\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + codexIncompleteMessageItem + "}\n\n" +
	"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"model\":\"gpt-5.4\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":16,\"total_tokens\":21}}}\n\n"

// A reply Codex cut short is a reply, not a dropped stream: each client gets
// it with its own reason for a cut-short reply, and no error. The executors
// used to wait for response.completed alone, so over HTTP a non-streaming
// request failed with a 408 and a stream ended in a 502 after the text.
func TestCodexExecutorPassesOnIncompleteResponse(t *testing.T) {
	for _, useWebsocket := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", useWebsocket), func(t *testing.T) {
			testCodexIncompleteResponse(t, useWebsocket)
		})
	}
}

func testCodexIncompleteResponse(t *testing.T, useWebsocket bool) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !useWebsocket {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(codexMaxOutputTokensSSE))
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// The websocket executor reads the reply from the final event alone, so
		// here that event carries the message.
		events := strings.Replace(codexMaxOutputTokensSSE, `"output":[],"usage"`, `"output":[`+codexIncompleteMessageItem+`],"usage"`, 1)
		for _, event := range strings.Split(strings.TrimSpace(events), "\n\n") {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.TrimPrefix(event, "data: ")))
		}
	}))
	defer server.Close()

	var executor cliproxyauth.ProviderExecutor = NewCodexExecutor(&config.Config{})
	if useWebsocket {
		executor = NewCodexWebsocketsExecutor(&config.Config{})
	}
	auth := &cliproxyauth.Auth{
		ID:         "codex-auth-incomplete-response",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL + "/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}

	for _, tc := range []struct {
		format  string
		request string
		// reply checks the non-streaming reply; stream names the text the
		// stream must carry.
		reply  func(gjson.Result) bool
		stream string
	}{
		{
			format:  "claude",
			request: `{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			reply: func(r gjson.Result) bool {
				return r.Get("stop_reason").String() == "max_tokens" && r.Get("content.0.text").String() == "Hello"
			},
			stream: `"stop_reason":"max_tokens"`,
		},
		{
			format:  "openai",
			request: `{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			reply: func(r gjson.Result) bool {
				return r.Get("choices.0.finish_reason").String() == "length" && r.Get("choices.0.message.content").String() == "Hello"
			},
			stream: `"finish_reason":"length"`,
		},
		{
			format:  "gemini",
			request: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}`,
			reply: func(r gjson.Result) bool {
				return r.Get("candidates.0.finishReason").String() == "MAX_TOKENS" && r.Get("candidates.0.content.parts.0.text").String() == "Hello"
			},
			stream: `"finishReason":"MAX_TOKENS"`,
		},
		{
			// Same protocol: the Responses client reads the event as it came.
			format:  "openai-response",
			request: `{"model":"gpt-5.4","input":"hi","max_output_tokens":16}`,
			reply: func(r gjson.Result) bool {
				return r.Get("status").String() == "incomplete" && r.Get("incomplete_details.reason").String() == "max_output_tokens"
			},
			stream: `"type":"response.incomplete"`,
		},
	} {
		format := sdktranslator.FromString(tc.format)
		t.Run(tc.format+"/non-stream", func(t *testing.T) {
			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model: "gpt-5.4", Payload: []byte(tc.request), Format: format,
			}, cliproxyexecutor.Options{SourceFormat: format})
			if err != nil {
				t.Fatalf("Execute() error = %v, want the cut-short reply", err)
			}
			if !tc.reply(gjson.ParseBytes(resp.Payload)) {
				t.Fatalf("reply = %s, want the text and the client's reason for a cut-short reply", resp.Payload)
			}
		})
		t.Run(tc.format+"/stream", func(t *testing.T) {
			payload := tc.request
			if tc.format != "gemini" {
				payload = strings.Replace(payload, `{`, `{"stream":true,`, 1)
			}
			stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
				Model: "gpt-5.4", Payload: []byte(payload), Format: format,
			}, cliproxyexecutor.Options{SourceFormat: format, Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var body strings.Builder
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error = %v, want the stream to end with the reply", chunk.Err)
				}
				body.Write(chunk.Payload)
			}
			if !strings.Contains(body.String(), tc.stream) {
				t.Fatalf("stream = %q, want it to carry %s", body.String(), tc.stream)
			}
		})
	}
}

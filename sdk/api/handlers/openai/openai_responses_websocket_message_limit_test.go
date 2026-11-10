package openai

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketDecodedMessageLimit(t *testing.T) {
	const limit = 1 << 20
	for _, compression := range []bool{false, true} {
		for _, messageType := range []int{websocket.TextMessage, websocket.BinaryMessage} {
			for _, generate := range []bool{false, true} {
				for _, delta := range []int{-1, 0, 1} {
					name := fmt.Sprintf("compressed_%t/type_%d/generate_%t/limit_%+d", compression, messageType, generate, delta)
					t.Run(name, func(t *testing.T) {
						exec := newWSScriptExecutor(t)
						url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{
							ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{MaxMessageMB: 1, MaxTranscriptMB: 4},
						})
						// A small write buffer also exercises fragmented messages. The
						// repeated input compresses far below the configured wire limit.
						dialer := &websocket.Dialer{EnableCompression: compression, WriteBufferSize: 512, HandshakeTimeout: 5 * time.Second}
						conn, response, err := dialer.Dial(url, nil)
						if response != nil && response.Body != nil {
							_ = response.Body.Close()
						}
						if err != nil {
							t.Fatalf("dial: %v", err)
						}
						t.Cleanup(func() { _ = conn.Close() })
						negotiated := response != nil && strings.Contains(response.Header.Get("Sec-Websocket-Extensions"), "permessage-deflate")
						if negotiated != compression {
							t.Fatalf("compression negotiated = %t, want %t", negotiated, compression)
						}
						input := func(padding string) string {
							return fmt.Sprintf(`[{"role":"user","content":[{"type":"input_text","text":%q}]}]`, padding)
						}
						extra := fmt.Sprintf(`"generate":%t`, generate)
						frame := wsScriptCreate(input(""), extra)
						frame = wsScriptCreate(input(strings.Repeat("x", limit+delta-len(frame))), extra)
						if len(frame) != limit+delta {
							t.Fatalf("fixture size = %d, want %d", len(frame), limit+delta)
						}
						_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
						writeErr := conn.WriteMessage(messageType, []byte(frame))
						_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						if delta > 0 {
							_, _, err := conn.ReadMessage()
							if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
								t.Fatalf("oversized decoded message must close with 1009, got %v (write: %v)", err, writeErr)
							}
							if n := len(exec.upstream(t)); n != 0 {
								t.Fatalf("oversized message reached the executor: %d calls", n)
							}
							return
						}
						if writeErr != nil {
							t.Fatalf("write allowed message: %v", writeErr)
						}
						if !generate {
							if event := wsTurnRead(t, conn); gjson.GetBytes(event, "type").String() != "response.created" {
								t.Fatalf("prewarm must start with response.created, got %s", event)
							}
						}
						if event := wsTurnRead(t, conn); gjson.GetBytes(event, "type").String() != "response.completed" {
							t.Fatalf("allowed message must complete, got %s", event)
						}
						wantCalls := 0
						if generate {
							wantCalls = 1
						}
						if n := len(exec.upstream(t)); n != wantCalls {
							t.Fatalf("executor calls = %d, want %d", n, wantCalls)
						}
						// Reaching the boundary must not leave the reader at an artificial
						// EOF or corrupt the next turn on the same connection.
						events := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("next")+"]", ""))
						if gjson.Get(events[len(events)-1], "type").String() != "response.completed" {
							t.Fatalf("next turn must complete, got %v", events)
						}
						if n := len(exec.upstream(t)); n != wantCalls+1 {
							t.Fatalf("executor calls after next turn = %d, want %d", n, wantCalls+1)
						}
					})
				}
			}
		}
	}
}

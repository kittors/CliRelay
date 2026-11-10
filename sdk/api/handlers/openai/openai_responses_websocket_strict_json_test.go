package openai

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

// A turn the gateway and the upstream would read differently is refused with
// an error event, as the same body is on POST (bodyutil.CheckJSONRequestBody),
// before it is pinned to an account or reaches an executor; the connection
// takes the next turn.
func TestResponsesWebsocketRefusesATurnParsersReadDifferently(t *testing.T) {
	exec := newWSScriptExecutor(t)
	url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{})
	conn, response, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).Dial(url, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	input := `[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]`
	write := func(frame string) {
		t.Helper()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for _, tc := range []struct{ name, frame, code string }{
		{"repeated input", wsScriptCreate(input, `"input":"forbidden","generate":true`), "duplicate_json_member"},
		{"repeated model", wsScriptCreate(input, `"model":"a-dearer-model","generate":true`), "duplicate_json_member"},
		{"NaN", wsScriptCreate(input, `"temperature":NaN,"generate":true`), "invalid_json"},
	} {
		write(tc.frame)
		event := wsTurnRead(t, conn)
		if gjson.GetBytes(event, "type").String() != "error" || gjson.GetBytes(event, "status").Int() != 400 || gjson.GetBytes(event, "error.code").String() != tc.code {
			t.Fatalf("%s: event = %s", tc.name, event)
		}
	}
	if n := len(exec.upstream(t)); n != 0 {
		t.Fatalf("a refused turn reached the executor: %d calls", n)
	}

	write(wsScriptCreate(input, `"generate":true`))
	if event := wsTurnRead(t, conn); gjson.GetBytes(event, "type").String() != "response.completed" {
		t.Fatalf("the next turn did not run: %s", event)
	}
	if n := len(exec.upstream(t)); n != 1 {
		t.Fatalf("executor calls = %d, want 1", n)
	}
}

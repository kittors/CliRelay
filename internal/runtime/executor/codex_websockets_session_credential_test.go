package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// The execution session is the client's Responses WebSocket, which moves to
// another credential when its pin is released. The upstream socket must move
// with it: it authenticated as the credential that opened it.
func TestCodexWebsocketSessionReconnectsWhenTheCredentialChanges(t *testing.T) {
	type handshake struct {
		authorization string
		firstType     string
	}
	var mu sync.Mutex
	var handshakes []handshake
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		index := -1
		for {
			_, message, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			mu.Lock()
			if index < 0 {
				handshakes = append(handshakes, handshake{authorization: r.Header.Get("Authorization"), firstType: gjson.GetBytes(message, "type").String()})
				index = len(handshakes) - 1
			}
			mu.Unlock()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{})
	defer executor.CloseExecutionSession("client-ws-1")
	format := sdktranslator.FromString("openai-response")
	run := func(token string) {
		t.Helper()
		auth := &cliproxyauth.Auth{
			ID:         "codex-" + token,
			Provider:   "codex",
			Status:     cliproxyauth.StatusActive,
			Attributes: map[string]string{"base_url": server.URL + "/backend-api/codex"},
			Metadata:   map[string]any{"access_token": token, "account_id": "account-" + token},
		}
		stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`), Format: format,
		}, cliproxyexecutor.Options{
			SourceFormat: format,
			Stream:       true,
			Metadata:     map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "client-ws-1"},
		})
		if err != nil {
			t.Fatalf("ExecuteStream(%s) error = %v", token, err)
		}
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatalf("ExecuteStream(%s) stream error = %v", token, chunk.Err)
			}
		}
	}

	run("token-a")
	run("token-a")
	run("token-b")

	mu.Lock()
	defer mu.Unlock()
	if len(handshakes) != 2 {
		t.Fatalf("upstream connections = %+v, want one per credential", handshakes)
	}
	if handshakes[0].authorization != "Bearer token-a" || handshakes[1].authorization != "Bearer token-b" {
		t.Fatalf("upstream connections = %+v, want the second one opened as token-b", handshakes)
	}
	if handshakes[1].firstType != "response.create" {
		t.Fatalf("first message on the new connection = %q, want response.create", handshakes[1].firstType)
	}
}

// A socket the session dropped may fail its read after the next request took
// the channel; only the socket the request reads may end it.
func TestCodexWebsocketSessionReadFailureOfADroppedSocket(t *testing.T) {
	sess := &codexWebsocketSession{}
	dropped, current := &websocket.Conn{}, &websocket.Conn{}
	readCh := make(chan codexWebsocketRead, 4)
	sess.setActive(readCh, current)

	sess.failActiveRead(dropped, errors.New("use of closed network connection"))
	select {
	case ev, ok := <-readCh:
		t.Fatalf("channel of the request on the current socket got %+v (open=%v), want nothing", ev, ok)
	default:
	}

	sess.failActiveRead(current, errors.New("upstream went away"))
	select {
	case ev := <-readCh:
		if ev.conn != current || ev.err == nil {
			t.Fatalf("read = %+v, want the current socket's failure", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("the current socket's failure did not reach the request")
	}
	if _, open := <-readCh; open {
		t.Fatal("channel still open after the current socket failed")
	}
}

// Over the websocket an exhausted allowance is an error event; the executor
// publishes its reset time as it does for the HTTP 429.
func TestParseCodexWebsocketErrorPublishesTheQuotaReset(t *testing.T) {
	payload := []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":7200},"headers":{"x-codex-primary-used-percent":"100","x-codex-primary-window-minutes":"300"}}`)
	err, ok := parseCodexWebsocketError(payload)
	if !ok {
		t.Fatal("parseCodexWebsocketError: not an error event")
	}
	var quota interface {
		RetryAfter() *time.Duration
		QuotaWindow() (string, int)
		StatusCode() int
	}
	if !errors.As(err, &quota) {
		t.Fatalf("error %T does not publish a retry wait", err)
	}
	if quota.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", quota.StatusCode())
	}
	if wait := quota.RetryAfter(); wait == nil || *wait != 2*time.Hour {
		t.Fatalf("RetryAfter = %v, want 2h", wait)
	}
	if window, minutes := quota.QuotaWindow(); window != "5h" || minutes != 300 {
		t.Fatalf("QuotaWindow = (%q, %d), want (5h, 300)", window, minutes)
	}
	if !strings.Contains(err.Error(), "usage_limit_reached") {
		t.Fatalf("error = %s, want the upstream body", err.Error())
	}
}

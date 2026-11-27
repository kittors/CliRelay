package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexWebsocketRecoversUnstoredContinuation(t *testing.T) {
	t.Run("idle disconnect", func(t *testing.T) { testUnstoredContinuation(t, false) })
	t.Run("write failure", func(t *testing.T) { testUnstoredContinuation(t, true) })
}

func testUnstoredContinuation(t *testing.T, failWrite bool) {
	t.Helper()
	upstream := newIncUpstream(t, []string{incReplyText1})
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := upstream.auth()
	auth.Attributes["websockets"] = "true"
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	const model = "websocket-continuation-test"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	handler := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	client, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	turn := func(prev string, messages ...string) gjson.Result {
		t.Helper()
		input := make([]map[string]string, 0, len(messages))
		for _, text := range messages {
			input = append(input, map[string]string{"role": "user", "content": text})
		}
		frame := map[string]any{"type": "response.create", "model": model, "store": false, "input": input}
		if prev != "" {
			frame["previous_response_id"] = prev
		}
		if err := client.WriteJSON(frame); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, payload, err := client.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			event := gjson.ParseBytes(payload)
			switch event.Get("type").String() {
			case "response.completed", "error":
				return event
			}
		}
	}
	id := func(event gjson.Result) string {
		t.Helper()
		if id := event.Get("response.id").String(); id != "" {
			return id
		}
		t.Fatalf("expected a completed response, got %s", event.Raw)
		return ""
	}
	first := id(turn("", "one"))
	upstream.closeNext.Store(!failWrite)
	second := id(turn(first, "two"))

	// Only one downstream WebSocket (and hence one execution session) is in use.
	exec.sessMu.Lock()
	var session *codexWebsocketSession
	for _, session = range exec.sessions {
		break
	}
	exec.sessMu.Unlock()
	if session == nil {
		t.Fatal("missing execution session")
	}
	if failWrite {
		// Exercise the write retry path even though the reader still sees a live socket.
		session.writeMu.Lock()
		session.connMu.Lock()
		err = session.conn.SetWriteDeadline(time.Now().Add(-time.Second))
		session.connMu.Unlock()
		session.writeMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	} else {
		// The idle deadline and a peer close both invalidate the socket in the reader.
		waitFor(t, func() bool {
			session.connMu.Lock()
			defer session.connMu.Unlock()
			return session.conn == nil
		}, "upstream reader to invalidate the closed socket")
	}

	failure := turn(second, "three")
	if conns, msgs, posts := upstream.counts(); conns != 1 || msgs != 2 || posts != 0 {
		t.Fatalf("stale continuation reached upstream: connections=%d messages=%d POSTs=%d", conns, msgs, posts)
	}
	if failure.Get("status").Int() != http.StatusBadRequest || failure.Get("error.code").String() != "previous_response_not_found" || failure.Get("error.param").String() != "previous_response_id" {
		t.Fatalf("stale continuation: %s", failure.Raw)
	}
	newID := id(turn("", "one", "two", "three"))
	id(turn(newID, "four"))
	msgs := upstream.websocketMessages()
	if len(msgs) != 4 || msgs[2].conn != 2 || msgs[3].conn != 2 ||
		gjson.Get(msgs[2].raw, "input.#").Int() != 3 || gjson.Get(msgs[2].raw, "previous_response_id").Exists() ||
		gjson.Get(msgs[3].raw, "previous_response_id").String() != newID {
		t.Fatalf("unexpected upstream replay/continuation: %+v", msgs)
	}
}

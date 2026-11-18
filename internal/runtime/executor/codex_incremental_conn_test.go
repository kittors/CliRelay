package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// A reply drained slowly must not stop the connection answering pings: the
// reader that answers them is the same one that queues the reply.
func TestCodexIncrementalConnAnswersPingsWhileTheTurnIsNotRead(t *testing.T) {
	var pongs atomic.Int64
	const messages = 500
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPongHandler(func(string) error { pongs.Add(1); return nil })
		// The reply follows the request, as on the backend: between turns the
		// connection drops what arrives.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		clientGone := make(chan struct{})
		go func() {
			defer close(clientGone)
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < messages; i++ {
			payload := fmt.Sprintf(`{"type":"response.output_text.delta","delta":"%s"}`, strings.Repeat("x", 1024))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				return
			}
		}
		for i := 0; i < 20; i++ {
			_ = conn.WriteControl(websocket.PingMessage, []byte("p"), time.Now().Add(time.Second))
			time.Sleep(10 * time.Millisecond)
		}
		<-clientGone
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{ID: "conn-test", Provider: "codex"}
	conn, resp, err := dialCodexIncrementalConn(context.Background(), &config.Config{}, auth, "ws"+strings.TrimPrefix(server.URL, "http"), codexIncrementalConnIdentity{authID: auth.ID}, http.Header{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	closeHTTPResponseBody(resp, "test")
	defer conn.close("test_done")
	sink, err := conn.beginTurn()
	if err != nil {
		t.Fatalf("beginTurn: %v", err)
	}
	if err := conn.send(context.Background(), []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Nobody reads the turn while the backend pings.
	waitFor(t, func() bool { return pongs.Load() >= 15 }, "pongs while the reply sits unread")

	for i := 0; i < messages; i++ {
		if _, err := sink.next(context.Background(), conn.done); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	conn.endTurn(sink)
}

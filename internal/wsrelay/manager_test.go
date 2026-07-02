package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsURLFromHTTP(serverURL, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func connectManagerClient(t *testing.T, mgr *Manager) (*websocket.Conn, func()) {
	t.Helper()

	server := httptest.NewServer(mgr.Handler())
	conn, resp, err := websocket.DefaultDialer.Dial(wsURLFromHTTP(server.URL, mgr.Path()), nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		server.Close()
		t.Fatalf("dial websocket: %v", err)
	}

	cleanup := func() {
		_ = conn.Close()
		server.Close()
	}
	return conn, cleanup
}

func countPendingRequests(s *session) int {
	if s == nil {
		return 0
	}
	count := 0
	s.pending.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func TestManagerSendCancelsPendingRequestWhenCallerContextEnds(t *testing.T) {
	mgr := NewManager(Options{
		ProviderFactory: func(*http.Request) (string, error) { return "provider-a", nil },
	})

	conn, cleanup := connectManagerClient(t, mgr)
	defer cleanup()

	waitForCondition(t, time.Second, func() bool {
		return mgr.session("provider-a") != nil
	}, "session was not registered")

	ctx, cancel := context.WithCancel(context.Background())
	respCh, err := mgr.Send(ctx, "provider-a", Message{
		ID:      "req-1",
		Type:    MessageTypeHTTPReq,
		Payload: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	var sent Message
	if err := conn.ReadJSON(&sent); err != nil {
		t.Fatalf("read outbound request: %v", err)
	}
	if sent.ID != "req-1" {
		t.Fatalf("sent request ID = %q, want req-1", sent.ID)
	}

	cancel()

	waitForCondition(t, time.Second, func() bool {
		select {
		case _, ok := <-respCh:
			return !ok
		default:
			return false
		}
	}, "response channel was not closed after context cancellation")

	waitForCondition(t, time.Second, func() bool {
		return countPendingRequests(mgr.session("provider-a")) == 0
	}, "pending request was not removed after context cancellation")
}

func TestManagerStopClosesPendingRequestsAndNotifiesDisconnect(t *testing.T) {
	var (
		disconnectedProvider string
		disconnectedErr      error
		mu                   sync.Mutex
	)

	mgr := NewManager(Options{
		ProviderFactory: func(*http.Request) (string, error) { return "provider-b", nil },
		OnDisconnected: func(provider string, err error) {
			mu.Lock()
			defer mu.Unlock()
			disconnectedProvider = provider
			disconnectedErr = err
		},
	})

	conn, cleanup := connectManagerClient(t, mgr)
	defer cleanup()

	waitForCondition(t, time.Second, func() bool {
		return mgr.session("provider-b") != nil
	}, "session was not registered")

	respCh, err := mgr.Send(context.Background(), "provider-b", Message{
		ID:      "req-stop",
		Type:    MessageTypeHTTPReq,
		Payload: map[string]any{"url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	var sent Message
	if err := conn.ReadJSON(&sent); err != nil {
		t.Fatalf("read outbound request: %v", err)
	}
	if sent.ID != "req-stop" {
		t.Fatalf("sent request ID = %q, want req-stop", sent.ID)
	}

	if err := mgr.Stop(context.Background()); err != nil {
		t.Fatalf("manager stop error: %v", err)
	}

	select {
	case msg, ok := <-respCh:
		if !ok {
			t.Fatal("response channel closed before error message was delivered")
		}
		if msg.Type != MessageTypeError {
			t.Fatalf("message type = %q, want %q", msg.Type, MessageTypeError)
		}
		if got := msg.Payload["error"]; got != "wsrelay: manager stopped" {
			t.Fatalf("error payload = %#v, want wsrelay: manager stopped", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stop error message")
	}

	select {
	case _, ok := <-respCh:
		if ok {
			t.Fatal("response channel should be closed after stop cleanup")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response channel close")
	}

	waitForCondition(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return disconnectedProvider == "provider-b" && disconnectedErr != nil && disconnectedErr.Error() == "wsrelay: manager stopped"
	}, "disconnect callback did not observe manager stop")
}

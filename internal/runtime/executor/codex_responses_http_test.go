package executor_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// Exercise the real executor, auth manager, SDK JSON validator and public HTTP
// handler together. The only substitute is a loopback upstream HTTP service.
func codexResponsesHTTPServer(t *testing.T, upstreamHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewCodexExecutor(&config.Config{}))
	auth := &coreauth.Auth{ID: "codex-http-framing-test", Provider: "codex", Status: coreauth.StatusActive, Attributes: map[string]string{"base_url": upstream.URL + "/backend-api/codex"}, Metadata: map[string]any{"access_token": "fixture", "account_id": "fixture"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5", Object: "model", OwnedBy: "openai"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	handler := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	return downstream
}

type codexResponsesHTTPEvent struct{ name, id, data string }

func codexResponsesHTTPParseEvents(raw string) []codexResponsesHTTPEvent {
	var result []codexResponsesHTTPEvent
	var current codexResponsesHTTPEvent
	var data []string
	s := bufio.NewScanner(strings.NewReader(raw))
	for s.Scan() {
		line := s.Text()
		if line == "" {
			if len(data) > 0 {
				current.data = strings.Join(data, "\n")
				result = append(result, current)
			}
			current = codexResponsesHTTPEvent{}
			data = nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			current.name = value
		case "id":
			current.id = value
		case "data":
			data = append(data, value)
		}
	}
	return result
}
func codexResponsesHTTPPost(t *testing.T, s *httptest.Server) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	r, err := client.Post(s.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","input":"fixture","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestCodexResponsesHTTPFrameBoundaries(t *testing.T) {
	const terminal = `{"type":"response.completed","response":{"output":[]}}`
	cases := []struct{ name, wire string }{
		{"late_metadata", "data: " + terminal + "\nevent: late\nid: final\n\n"},
		{"empty_surrounding", "event: late\ndata:\ndata: " + terminal + "\ndata:\nid: final\n\n"},
		{"fragmented", "data: {\ndata: \"type\":\"response.completed\",\ndata: \"response\":{\"output\":[]}}\nevent: late\nid: final\n\n"},
		{"clean_eof", "data: " + terminal + "\nevent: late\nid: final\n"},
		{"crlf", "data: " + terminal + "\r\nevent: late\r\nid: final\r\n\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := codexResponsesHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.wire)
			})
			resp := codexResponsesHTTPPost(t, s)
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			events := codexResponsesHTTPParseEvents(string(b))
			if resp.StatusCode != 200 || len(events) != 1 || events[0].name != "late" || events[0].id != "final" || !gjson.Valid(events[0].data) || gjson.Get(events[0].data, "type").String() != "response.completed" {
				t.Fatalf("status=%d events=%+v wire=%q", resp.StatusCode, events, b)
			}
		})
	}
}
func TestCodexResponsesHTTPImageSynthetic(t *testing.T) {
	s := codexResponsesHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"status\":\"completed\",\"result\":\"AAAA\",\"output_format\":\"png\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\nevent: response.completed\nid: terminal\n\n")
	})
	resp := codexResponsesHTTPPost(t, s)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := codexResponsesHTTPParseEvents(string(b))
	if len(events) < 3 {
		t.Fatalf("missing synthetic: %q", b)
	}
	for _, e := range events {
		if !gjson.Valid(e.data) || e.name != gjson.Get(e.data, "type").String() {
			t.Fatalf("invalid event %+v", e)
		}
	}
	if last := events[len(events)-1]; last.name != "response.completed" || last.id != "terminal" {
		t.Fatalf("terminal %+v", last)
	}
}

func TestCodexResponsesHTTPReadErrorDiscardsPartialEvent(t *testing.T) {
	s := codexResponsesHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		// An intentionally short Content-Length turns socket EOF into a read error.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "10000")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\nevent: unfinished\n")
	})
	resp := codexResponsesHTTPPost(t, s)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := codexResponsesHTTPParseEvents(string(b))
	if len(events) != 2 || events[0].name != "response.created" || events[1].name != "error" {
		t.Fatalf("events=%+v wire=%q", events, b)
	}
	if strings.Contains(string(b), "unfinished") || strings.Contains(string(b), "response.completed") {
		t.Fatalf("partial terminal leaked: %q", b)
	}
}

func TestCodexResponsesHTTPCancelDiscardsPartialEvent(t *testing.T) {
	upstreamStopped := make(chan struct{})
	s := codexResponsesHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamStopped)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ready\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\nevent: unfinished\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"fixture","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != ": ready\n" {
		t.Fatalf("heartbeat=%q err=%v", line, err)
	}
	cancel()
	select {
	case <-upstreamStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("client cancellation did not reach upstream")
	}
}

func TestCodexResponsesHTTPMalformedTerminal(t *testing.T) {
	s := codexResponsesHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\ndata: {}\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	})
	resp := codexResponsesHTTPPost(t, s)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := codexResponsesHTTPParseEvents(string(b))
	if len(events) != 2 || events[1].name != "error" {
		t.Fatalf("events=%+v wire=%q", events, b)
	}
	for _, e := range events {
		if gjson.Get(e.data, "type").String() == "response.completed" {
			t.Fatalf("malformed terminal accepted: %q", b)
		}
	}
}

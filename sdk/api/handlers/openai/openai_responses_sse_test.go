package openai

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// A blank line resets an SSE event's type even when it has no data and is not dispatched.
func responsesSSETypes(raw string) []string {
	var out []string
	name := ""
	hasData := false
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if hasData {
				if name == "" {
					name = "message"
				}
				out = append(out, name)
			}
			name = ""
			hasData = false
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			hasData = true
		}
	}
	return out
}
func TestResponsesSSELoneEventKeepsNameWithItsData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"split_event_data", []string{"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"x"}`}, "response.output_text.delta"},
		{"split_event_data_with_comment", []string{"event: response.output_text.delta", ": keepalive", `data: {"type":"response.output_text.delta","delta":"x"}`}, "response.output_text.delta"},
		{"split_empty_data_prefix", []string{"event: response.output_text.delta", "data:", `data: {"type":"response.output_text.delta","delta":"x"}`}, "response.output_text.delta"},
		{"split_whitespace_data_prefix", []string{"event: response.output_text.delta", "data:  ", `data: {"type":"response.output_text.delta","delta":"x"}`}, "response.output_text.delta"},
		{"split_trailing_empty_data", []string{"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"x"}`, "data:"}, "response.output_text.delta"},
		{"complete_pair_control", []string{"event: response.output_text.delta\ndata: {\"delta\":\"x\"}"}, "response.output_text.delta"},
		{"data_only_control", []string{`data: {"delta":"x"}`}, "message"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := make(chan []byte, len(test.chunks))
			for _, chunk := range test.chunks {
				data <- []byte(chunk)
			}
			close(data)
			errs := make(chan *interfaces.ErrorMessage)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/v1/responses", nil)
			handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			handler.forwardResponsesStream(ctx, ctx.Writer, func(error) {}, data, errs)
			got := responsesSSETypes(recorder.Body.String())
			if len(got) != 1 || got[0] != test.want {
				t.Errorf("dispatched event types=%q, want [%s]; wire=%q", got, test.want, recorder.Body.String())
			}
		})
	}
}

func TestResponsesSSEChunkFraming(t *testing.T) {
	for _, tc := range []struct{ name, chunk, want string }{
		{"empty", "", ""},
		{"event only", "event: delta", "event: delta\n"},
		{"event LF", "event: delta\n", "event: delta\n"},
		{"id only", "id: 12", "id: 12\n"},
		{"comment only", ": heartbeat", ": heartbeat\n"},
		{"retry only", "retry: 50", "retry: 50\n"},
		{"unknown field", "metadata: data: x", "metadata: data: x\n"},
		{"data no suffix", "data: {}", "data: {}\n\n"},
		{"data LF", "data: {}\n", "data: {}\n\n"},
		{"data CRLF", "data: {}\r\n", "data: {}\r\n\n"},
		{"data CR", "data: {}\r", "data: {}\r\n\n"},
		{"framed LF", "data: {}\n\n", "data: {}\n\n"},
		{"framed CRLF", "data: {}\r\n\r\n", "data: {}\r\n\r\n"},
		{"framed CR", "data: {}\r\r", "data: {}\r\r"},
		{"empty data", "data:", "data:\n"},
		{"whitespace data", "data:  ", "data:  \n"},
		{"explicit empty data event", "data:\n\n", "data:\n\n"},
		{"nonempty then empty data", "data: {}\ndata:", "data: {}\ndata:\n\n"},
		{"colonless empty data", "data", "data\n"},
		{"data in comment", ": data: fake", ": data: fake\n"},
		{"leading whitespace is not a field", " data: fake", " data: fake\n"},
		{"multi data in event", "event: delta\ndata: one\ndata: two", "event: delta\ndata: one\ndata: two\n\n"},
		{"late metadata in event", "data: {}\nevent: delta\nid: 12", "data: {}\nevent: delta\nid: 12\n\n"},
		{"several events", "event: one\ndata: 1\n\nevent: two\ndata: 2", "event: one\ndata: 1\n\nevent: two\ndata: 2\n\n"},
		{"completed then pending event", "data: 1\n\nevent: two", "data: 1\n\nevent: two\n"},
		{"completed then comment", "data: 1\n\n: heartbeat\n", "data: 1\n\n: heartbeat\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			writeResponsesSSEChunk(&out, []byte(tc.chunk))
			if got := out.String(); got != tc.want {
				t.Fatalf("wire=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponsesSSEMetadataSurvivesSplitChunks(t *testing.T) {
	var out strings.Builder
	for _, chunk := range []string{"id: 12\n", "retry: 50", ": comment", "event: delta\n", "data: {}"} {
		writeResponsesSSEChunk(&out, []byte(chunk))
	}
	want := "id: 12\nretry: 50\n: comment\nevent: delta\ndata: {}\n\n"
	if out.String() != want {
		t.Fatalf("wire=%q, want %q", out.String(), want)
	}
}

func TestResponsesSSESplitEventDispatchesBeforeProviderCloses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	data := make(chan []byte, 1)
	errs := make(chan *interfaces.ErrorMessage)
	data <- []byte("event: response.output_text.delta")
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		h.forwardResponsesStream(c, c.Writer, func(error) {}, data, errs)
	}))
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: response.output_text.delta\n" {
		t.Fatalf("metadata line=%q, err=%v", line, err)
	}
	// Supply the data only after the client's reader has seen the separate
	// event line. An extra delimiter here would discard that event name.
	data <- []byte(`data: {"type":"response.output_text.delta","delta":"fixture"}`)
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data:") {
		t.Fatalf("data line=%q, err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "\n" {
		t.Fatalf("event delimiter=%q, err=%v; provider remains open", line, err)
	}
}

func TestResponsesSSEClientCancellationStopsForwarder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, chunk := range []string{"event: delta", "data: {}"} {
		t.Run(chunk, func(t *testing.T) {
			data := make(chan []byte, 1)
			data <- []byte(chunk)
			errs := make(chan *interfaces.ErrorMessage)
			stopped := make(chan error, 1)
			h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := gin.CreateTestContext(w)
				c.Request = r
				h.forwardResponsesStream(c, c.Writer, func(err error) { stopped <- err }, data, errs)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			client := &http.Client{Timeout: time.Second}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			cancel()
			select {
			case err = <-stopped:
				if err == nil {
					t.Fatal("cancel callback lost client cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("forwarder did not stop after client cancellation")
			}
		})
	}
}

func TestResponsesSSEEmptyDataPrefixKeepsOneEvent(t *testing.T) {
	for _, prefix := range []string{"data:", "data: ", "data:  ", "data", "data:\n"} {
		t.Run(prefix, func(t *testing.T) {
			var out strings.Builder
			writeResponsesSSEChunk(&out, []byte("event: delta"))
			writeResponsesSSEChunk(&out, []byte(prefix))
			writeResponsesSSEChunk(&out, []byte(`data: {"x":1}`))
			line := prefix
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			want := "event: delta\n" + line + "data: {\"x\":1}\n\n"
			if out.String() != want {
				t.Fatalf("empty data prefix split the event: wire=%q, want=%q", out.String(), want)
			}
		})
	}
}

func TestResponsesSSETrailingEmptyDataDoesNotDispatchBeforeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	go func() {
		for _, chunk := range []string{"event: delta", "data: {}", "data:"} {
			data <- []byte(chunk)
		}
		errs <- &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("provider failed")}
	}()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	h.forwardResponsesStream(ctx, ctx.Writer, func(error) {}, data, errs)
	got := responsesSSETypes(recorder.Body.String())
	if len(got) != 2 || got[0] != "delta" || got[1] != "error" {
		t.Fatalf("event types=%q, want [delta error]; wire=%q", got, recorder.Body.String())
	}
}

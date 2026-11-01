package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// A turn where the model only returns a picture: nothing inlines it into assistant text,
// so the normalizer emits the synthetic display item when response.completed arrives.
const codexImageOnlyTurnSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"

type framedSSEEvent struct {
	name string
	data string
}

// parseFramedSSE follows the SSE rules: "data:" lines accumulate, joined by "\n", until a
// blank line dispatches the event.
func parseFramedSSE(raw string) []framedSSEEvent {
	var (
		events []framedSSEEvent
		name   string
		data   []string
	)
	dispatch := func() {
		if len(data) > 0 {
			events = append(events, framedSSEEvent{name: name, data: strings.Join(data, "\n")})
		}
		name, data = "", nil
	}
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	dispatch()
	return events
}

// streamCodexResponses runs upstream through ExecuteStream for a Responses client and
// returns the chunks it forwards, one line each.
func streamCodexResponses(t *testing.T, upstream string) []string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstream))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "codex-auth-image-framing",
		Provider:   "codex",
		Status:     cliproxyauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL + "/backend-api/codex"},
		Metadata:   map[string]any{"access_token": "token", "account_id": "account-1"},
	}
	stream, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"draw a cat","stream":true}`),
		Format:  sdktranslator.FromString("openai-response"),
	}, cliproxyexecutor.Options{
		Stream:       true,
		SourceFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var chunks []string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	return chunks
}

// assertWellFramed checks every event carries one JSON document whose type matches the
// event name. Chunks are joined as bare lines: the check does not lean on the blank line
// the Responses handler writes ahead of each "event:" line.
func assertWellFramed(t *testing.T, chunks []string) []framedSSEEvent {
	t.Helper()
	events := parseFramedSSE(strings.Join(chunks, "\n") + "\n")
	for i, event := range events {
		if !gjson.Valid(event.data) {
			t.Fatalf("event %d (%q) data is not one JSON document: %.300q", i, event.name, event.data)
		}
		if eventType := gjson.Get(event.data, "type").String(); event.name != "" && event.name != eventType {
			t.Fatalf("event %d is named %q but carries type %q", i, event.name, eventType)
		}
	}
	return events
}

func TestCodexExecutorExecuteStreamFramesImageDisplayFallbackAsOwnEvent(t *testing.T) {
	events := assertWellFramed(t, streamCodexResponses(t, codexImageOnlyTurnSSE))

	var types []string
	for _, event := range events {
		types = append(types, gjson.Get(event.data, "type").String())
	}
	want := []string{"response.created", "response.output_item.done", "response.output_item.done", "response.completed"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	fallback := events[2]
	if fallback.name != "response.output_item.done" {
		t.Fatalf("fallback event name = %q, want response.output_item.done", fallback.name)
	}
	if got := gjson.Get(fallback.data, "item.id").String(); got != "msg_image_display" {
		t.Fatalf("fallback item.id = %q, want msg_image_display", got)
	}
	if got := gjson.Get(fallback.data, "item.content.0.text").String(); !strings.Contains(got, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("fallback text = %q, want the image data URL", got)
	}
	completed := events[3]
	if completed.name != "response.completed" || gjson.Get(completed.data, "response.status").String() != "completed" {
		t.Fatalf("terminal event = %q %.200s, want an intact response.completed", completed.name, completed.data)
	}
}

func TestCodexExecutorExecuteStreamFramesImageDisplayFallbackWithoutEventLines(t *testing.T) {
	var dataOnly strings.Builder
	for _, line := range strings.Split(codexImageOnlyTurnSSE, "\n") {
		if !strings.HasPrefix(line, "event:") {
			dataOnly.WriteString(line + "\n")
		}
	}
	events := assertWellFramed(t, streamCodexResponses(t, dataOnly.String()))
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4 (created, image, fallback, completed)", len(events))
	}
	if got := gjson.Get(events[3].data, "type").String(); got != "response.completed" {
		t.Fatalf("last event type = %q, want response.completed", got)
	}
}

func TestCodexExecutorExecuteStreamLeavesStreamsWithoutFallbackUnchanged(t *testing.T) {
	upstream := strings.Replace(codexImageOnlyTurnSSE,
		`"type":"image_generation_call","status":"completed","result":"iVBORw0KGgo=","output_format":"png"`,
		`"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi"}]`, 1)
	chunks := streamCodexResponses(t, upstream)
	want := strings.Split(strings.TrimSuffix(upstream, "\n"), "\n")
	if strings.Join(chunks, "\n") != strings.Join(want, "\n") {
		t.Fatalf("forwarded lines changed:\n got %q\nwant %q", chunks, want)
	}
}

func TestFrameCodexImageStreamEvents(t *testing.T) {
	held := []byte("event: response.completed")
	completed := []byte(`data: {"type":"response.completed","response":{}}`)
	fallback := []byte(`data: {"type":"response.output_item.done","item":{"id":"msg_image_display"}}`)

	tests := []struct {
		name   string
		held   []byte
		events [][]byte
		want   []string
	}{
		{name: "nothing", want: nil},
		{name: "held event line only", held: held, want: []string{string(held)}},
		{name: "plain line", events: [][]byte{completed}, want: []string{string(completed)}},
		{name: "plain line after event line", held: held, events: [][]byte{completed}, want: []string{string(held), string(completed)}},
		{
			name:   "synthetic event goes out whole before the held event line",
			held:   held,
			events: [][]byte{fallback, completed},
			want:   []string{"event: response.output_item.done", string(fallback), "", string(held), string(completed)},
		},
		{
			name:   "synthetic event without a held event line",
			events: [][]byte{fallback, completed},
			want:   []string{"event: response.output_item.done", string(fallback), "", string(completed)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := frameCodexImageStreamEvents(tt.held, tt.events)
			if len(got) != len(tt.want) {
				t.Fatalf("lines = %q, want %q", got, tt.want)
			}
			for i := range got {
				if !bytes.Equal(got[i], []byte(tt.want[i])) {
					t.Fatalf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

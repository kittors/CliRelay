package executor

import (
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestCodexImageFallbackPreservesTerminalEventWithMetadata(t *testing.T) {
	for _, extra := range []string{": keep-alive\n", "id: terminal-1\n", "retry: 1000\n", "event: response.completed\n"} {
		t.Run(strings.TrimSpace(extra), func(t *testing.T) {
			upstream := strings.Replace(codexImageOnlyTurnSSE, "event: response.completed\n", "event: response.completed\n"+extra, 1)
			chunks := streamCodexResponses(t, upstream)
			for _, handlerFormatting := range []bool{false, true} {
				wire := strings.Join(chunks, "\n") + "\n"
				if handlerFormatting {
					var b strings.Builder
					for _, chunk := range chunks {
						if strings.HasPrefix(chunk, "event:") {
							b.WriteString("\n")
						}
						b.WriteString(chunk)
						b.WriteString("\n")
					}
					b.WriteString("\n")
					wire = b.String()
				}
				events := parseFramedSSE(wire)
				if len(events) != 4 {
					t.Fatalf("handler=%v events=%d, want 4", handlerFormatting, len(events))
				}
				if events[3].name != "response.completed" {
					t.Errorf("handler=%v final event name=%q, want response.completed; final data=%s", handlerFormatting, events[3].name, events[3].data)
				}
			}
		})
	}
}

func TestCodexImageFallbackHandlesTerminalFieldOrderingAndMultilineJSON(t *testing.T) {
	const terminal = `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	cases := map[string]string{
		"event after data":                terminal + "\nevent: response.completed\n\n",
		"id before event":                 "id: terminal-1\nevent: response.completed\n" + terminal + "\n\n",
		"empty data before payload":       "event: response.completed\ndata:\n" + terminal + "\n\n",
		"multiline JSON":                  "event: response.completed\ndata: {\ndata: \"type\":\"response.completed\",\ndata: \"response\":{\"status\":\"completed\",\"output\":[]}}\n\n",
		"duplicate event uses last value": "event: wrong\nevent: response.completed\n" + terminal + "\n\n",
		"clean EOF":                       "event: response.completed\n" + terminal,
	}
	for name, ending := range cases {
		t.Run(name, func(t *testing.T) {
			start := strings.Index(codexImageOnlyTurnSSE, "event: response.completed\n")
			chunks := streamCodexResponses(t, codexImageOnlyTurnSSE[:start]+ending)
			for _, wire := range []string{strings.Join(chunks, "\n") + "\n", codexResponsesWire(chunks)} {
				events := parseFramedSSE(wire)
				if len(events) != 4 {
					t.Fatalf("events=%d, want 4; wire=%q", len(events), wire)
				}
				if events[2].name != "response.output_item.done" || events[3].name != "response.completed" {
					t.Fatalf("event names=%q/%q", events[2].name, events[3].name)
				}
				for _, event := range events {
					if !gjson.Valid(event.data) {
						t.Fatalf("invalid data: %q", event.data)
					}
				}
			}
		})
	}
}

// Match forwardResponsesStream's real output framing, including its leading newline
// for event: chunks. This catches valid late event fields being split by that writer.
func codexResponsesWire(chunks []string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		if strings.HasPrefix(chunk, "event:") {
			b.WriteByte('\n')
		}
		b.WriteString(chunk)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

func TestCodexImageSSEFramerPreservesUnchangedLinesAndStreamsImmediately(t *testing.T) {
	f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer()}
	lines := []string{": leading heartbeat", "id: 1", "event: response.created", ": between", "retry: 1000", `data:{"type":"response.created","response":{}}`, "", "", "event: trailing"}
	var got []string
	for i, line := range lines {
		out, err := f.push([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range out {
			got = append(got, string(v))
		}
		if i == 0 && len(out) != 1 {
			t.Fatal("leading heartbeat was buffered")
		}
		if i == 5 && len(out) == 0 {
			t.Fatal("ordinary payload waits for blank delimiter")
		}
	}
	for _, v := range f.finish(false) {
		got = append(got, string(v))
	}
	if strings.Join(got, "\n") != strings.Join(lines, "\n") {
		t.Fatalf("got=%q want=%q", got, lines)
	}
}

func TestCodexImageSSEFramerBoundsBufferedMetadataAndTerminal(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer(), limit: 100}
		if terminal {
			f.synthetic = [][]byte{[]byte(`data: {"type":"response.output_item.done"}`)}
		}
		for i := 0; i < 3; i++ {
			if _, err := f.push([]byte("event:")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.push([]byte("id: x")); err == nil {
			t.Fatalf("terminal=%v: expected aggregate overflow", terminal)
		}
	}
	f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer(), limit: 30}
	if _, err := f.push([]byte("event:")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.push(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.push([]byte("event:")); err != nil {
		t.Fatalf("budget not reset: %v", err)
	}
}

func TestCodexImageSSEFramerKeepsTerminalIDOffSyntheticEvent(t *testing.T) {
	upstream := strings.Replace(codexImageOnlyTurnSSE, "event: response.completed\n", "id: terminal-1\nevent: response.completed\n", 1)
	wire := codexResponsesWire(streamCodexResponses(t, upstream))
	fallback := strings.Index(wire, "msg_image_display")
	id := strings.Index(wire, "id: terminal-1")
	completed := strings.LastIndex(wire, `"type":"response.completed"`)
	if fallback < 0 || id < fallback || id > completed {
		t.Fatalf("terminal ID not retained on original event: %q", wire)
	}
}

func TestCodexImageSSEFramerPreservesMultilineAndInvalidData(t *testing.T) {
	for _, lines := range [][]string{
		{"event: response.created", "data: {", `data: "type":"response.created",`, `data: "response":{}}`, ""},
		{"event: unknown", "data: not JSON", ": comment", "data: still not JSON", ""},
		{"data: [DONE]", ""},
	} {
		f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer()}
		var got []string
		for _, line := range lines {
			out, err := f.push([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range out {
				got = append(got, string(v))
			}
		}
		if strings.Join(got, "\n") != strings.Join(lines, "\n") {
			t.Fatalf("got=%q want=%q", got, lines)
		}
	}
}

func TestCodexImageSSEFramerDefersFragmentedJSONUntilDelimiter(t *testing.T) {
	f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer()}
	for _, line := range []string{"data: {", `data: "type":"response.created"`} {
		if out, err := f.push([]byte(line)); err != nil || len(out) > 0 {
			t.Fatalf("out=%q err=%v", out, err)
		}
	}
	// Exercise thousands of tiny fields. These must be accumulated, not joined and
	// revalidated on every push; even the closing JSON waits for one final parse.
	for i := 0; i < 10000; i++ {
		if out, err := f.push([]byte("data: ")); err != nil || len(out) > 0 {
			t.Fatalf("out=%q err=%v", out, err)
		}
	}
	if out, err := f.push([]byte("data: }")); err != nil || len(out) > 0 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if out, err := f.push(nil); err != nil || len(out) != 10004 {
		t.Fatalf("lines=%d err=%v", len(out), err)
	}
}

type codexSSEInterruptedBody struct {
	reader        *strings.Reader
	ctx           context.Context
	wait          bool
	readErr       error
	exhausted     chan struct{}
	closed        chan struct{}
	exhaustedOnce sync.Once
	closeOnce     sync.Once
}

func (b *codexSSEInterruptedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err != io.EOF {
		return n, err
	}
	b.exhaustedOnce.Do(func() { close(b.exhausted) })
	if b.wait {
		<-b.ctx.Done()
	}
	return 0, b.readErr
}
func (b *codexSSEInterruptedBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestCodexImageStreamCancellationClosesBodyWithoutConsumer(t *testing.T) {
	for _, pendingTerminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "abandoned consumer", true: "buffered terminal"}[pendingTerminal], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &codexSSEInterruptedBody{reader: strings.NewReader(strings.TrimRight(codexImageOnlyTurnSSE, "\n") + "\n"), ctx: ctx, wait: true, readErr: context.Canceled, exhausted: make(chan struct{}), closed: make(chan struct{})}
			stream := codexStreamWithBody(t, ctx, body)
			if pendingTerminal {
				// Drain only the two already-complete upstream events; leave the pending
				// terminal undelimited and abandon the consumer before cancellation.
				blanks := 0
				for blanks < 2 {
					select {
					case chunk := <-stream.Chunks:
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						if len(chunk.Payload) == 0 {
							blanks++
						}
					case <-time.After(2 * time.Second):
						t.Fatal("earlier events did not stream")
					}
				}
				select {
				case <-body.exhausted:
				case <-time.After(2 * time.Second):
					t.Fatal("terminal was not buffered")
				}
			}
			cancel()
			// This waits without receiving from Chunks: an unconditional payload or error
			// send would strand the worker and its deferred Body.Close here.
			select {
			case <-body.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("canceled worker blocked on abandoned consumer")
			}
			for chunk := range stream.Chunks {
				if strings.Contains(string(chunk.Payload), "msg_image_display") || strings.Contains(string(chunk.Payload), `"type":"response.completed"`) {
					t.Fatalf("flushed interrupted terminal: %s", chunk.Payload)
				}
			}
		})
	}
}

func TestCodexImageStreamReadErrorDoesNotFlushPendingTerminal(t *testing.T) {
	wantErr := errors.New("upstream interrupted")
	body := &codexSSEInterruptedBody{reader: strings.NewReader(strings.TrimRight(codexImageOnlyTurnSSE, "\n") + "\n"), ctx: context.Background(), readErr: wantErr, exhausted: make(chan struct{}), closed: make(chan struct{})}
	stream := codexStreamWithBody(t, context.Background(), body)
	var gotErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
		if strings.Contains(string(chunk.Payload), "msg_image_display") || strings.Contains(string(chunk.Payload), `"type":"response.completed"`) {
			t.Fatalf("flushed interrupted terminal: %s", chunk.Payload)
		}
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error=%v want=%v", gotErr, wantErr)
	}
}

func codexStreamWithBody(t *testing.T, ctx context.Context, body io.ReadCloser) *cliproxyexecutor.StreamResult {
	t.Helper()
	ctx = cliproxyexecutor.WithRoundTripper(ctx, roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}))
	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "codex-auth-image-cancel", Provider: "codex", Status: cliproxyauth.StatusActive, Metadata: map[string]any{"access_token": "token", "account_id": "account-1"}}
	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"draw a cat","stream":true}`), Format: sdktranslator.FromString("openai-response")}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestParseFramedSSERequiresBlankDelimiter(t *testing.T) {
	if events := parseFramedSSE("data: {}\n"); len(events) != 0 {
		t.Fatalf("EOF dispatched an incomplete event: %v", events)
	}
	events := parseFramedSSE("event: ignored\r\nevent: actual\r\ndata: {\r\ndata: }\r\n\r\n")
	if len(events) != 1 || events[0].name != "actual" || events[0].data != "{\n}" {
		t.Fatalf("unexpected parsed events: %v", events)
	}
}

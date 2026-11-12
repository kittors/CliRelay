package executor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCodexResponsesFramesPreserveLateFieldsAndEmptyData(t *testing.T) {
	const payload = `{"type":"response.completed","response":{"output":[]}}`
	for _, fields := range []string{
		"data: " + payload + "\nevent: late\nid: last\n: tail\n",
		"event: late\ndata:\ndata: " + payload + "\ndata:\nid: last\n",
		"data: {\ndata: \"type\":\"response.completed\",\ndata: \"response\":{\"output\":[]}}\nevent: late\nid: last\n",
	} {
		for _, ending := range []string{"\n", ""} {
			chunks := streamCodexResponses(t, fields+ending)
			if len(chunks) != 1 {
				t.Fatalf("chunks=%q", chunks)
			}
			events := parseFramedSSE(chunks[0])
			if len(events) != 1 || events[0].name != "late" {
				t.Fatalf("events=%+v wire=%q", events, chunks[0])
			}
			if !strings.Contains(chunks[0], "id: last\n") {
				t.Fatal("lost late id")
			}
		}
	}
}

func TestCodexResponsesWaitsForBlankButStreamsHeartbeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := codexStreamWithBody(t, ctx, reader)
	put := func(s string) { t.Helper(); go func() { _, _ = io.WriteString(writer, s) }() }
	take := func() string {
		t.Helper()
		select {
		case c := <-stream.Chunks:
			if c.Err != nil {
				t.Fatal(c.Err)
			}
			return string(c.Payload)
		case <-time.After(time.Second):
			t.Fatal("no chunk")
			return ""
		}
	}
	put(": heartbeat\n")
	if got := take(); got != ": heartbeat\n" {
		t.Fatalf("heartbeat=%q", got)
	}
	put("event: first\ndata: {\"type\":\"response.created\"}\n")
	select {
	case c := <-stream.Chunks:
		t.Fatalf("undelimited dispatched: %q", c.Payload)
	case <-time.After(30 * time.Millisecond):
	}
	put("event: late\nid: last\n\n")
	got := take()
	if !strings.HasSuffix(got, "id: last\n\n") || parseFramedSSE(got)[0].name != "late" {
		t.Fatalf("frame=%q", got)
	}
	put("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	_ = take()
	writer.Close()
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
	}
}

func TestCodexResponsesFramerBoundsAllEventBytes(t *testing.T) {
	f := codexImageSSEFramer{normalizer: newCodexImageStreamNormalizer(), waitForDelimiter: true, limit: 100}
	if out, err := f.push([]byte(`data: {"type":"response.created"}`)); err != nil || len(out) != 0 {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if _, err := f.push([]byte("id: " + strings.Repeat("x", 100))); err == nil {
		t.Fatal("late fields bypass cap")
	}
}

func TestCodexResponsesMalformedTerminalDoesNotComplete(t *testing.T) {
	for _, event := range []string{
		"data: {\"type\":\"response.completed\",\"response\":",
		"data: {}\ndata: {\"type\":\"response.completed\",\"response\":{}}",
	} {
		for _, ending := range []string{"\n\n", ""} {
			body := io.NopCloser(strings.NewReader(event + ending))
			stream := codexStreamWithBody(t, context.Background(), body)
			var err error
			for c := range stream.Chunks {
				if len(c.Payload) > 0 {
					t.Fatalf("malformed frame escaped: %q", c.Payload)
				}
				if c.Err != nil {
					err = c.Err
				}
			}
			if err == nil {
				t.Fatal("malformed terminal treated as successful")
			}
		}
	}
}

func TestCodexResponsesReadErrorDoesNotFlushOrdinaryPartial(t *testing.T) {
	want := errors.New("interrupted")
	body := &codexSSEInterruptedBody{reader: strings.NewReader("data: {\"type\":\"response.created\"}\n"), ctx: context.Background(), readErr: want, exhausted: make(chan struct{}), closed: make(chan struct{})}
	stream := codexStreamWithBody(t, context.Background(), body)
	var err error
	for c := range stream.Chunks {
		if len(c.Payload) > 0 {
			t.Fatalf("flushed partial: %q", c.Payload)
		}
		if c.Err != nil {
			err = c.Err
		}
	}
	if !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
}

func TestCodexResponsesCancellationDoesNotFlushOrdinaryPartial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &codexSSEInterruptedBody{reader: strings.NewReader("data: {\"type\":\"response.created\"}\n"), ctx: ctx, wait: true, readErr: context.Canceled, exhausted: make(chan struct{}), closed: make(chan struct{})}
	stream := codexStreamWithBody(t, ctx, body)
	select {
	case <-body.exhausted:
	case <-time.After(time.Second):
		t.Fatal("ordinary event wasn't buffered")
	}
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("canceled stream retained body")
	}
	for c := range stream.Chunks {
		if len(c.Payload) > 0 {
			t.Fatalf("flushed canceled event: %q", c.Payload)
		}
	}
}

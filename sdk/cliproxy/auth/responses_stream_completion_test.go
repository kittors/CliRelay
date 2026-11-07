package auth

import (
	"net/http"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestResponsesStreamCompletionTrackerAcceptsCompletedWithoutSSEDelimiter(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: response.created\ndata: {\"type\":\"response.created\"}"))
	tracker.Observe([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}"))

	if err := tracker.ErrIfIncomplete(); err != nil {
		t.Fatalf("ErrIfIncomplete() error = %v, want nil", err)
	}
}

func TestResponsesStreamCompletionTrackerAcceptsDataOnlyCompletedChunk(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("data: {\"type\":\"response.created\"}"))
	tracker.Observe([]byte("data: {\"type\":\"response.completed\"}"))

	if err := tracker.ErrIfIncomplete(); err != nil {
		t.Fatalf("ErrIfIncomplete() error = %v, want nil", err)
	}
}

func TestResponsesStreamCompletionTrackerReturnsResponseFailedError(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"Rate limit reached\"}}}"))

	err := tracker.ErrIfIncomplete()
	if err == nil {
		t.Fatal("ErrIfIncomplete() error = nil, want response.failed error")
	}
	if strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("ErrIfIncomplete() error = %v, should preserve upstream failure instead of incomplete stream", err)
	}
	if !strings.Contains(err.Error(), "Rate limit reached") {
		t.Fatalf("ErrIfIncomplete() error = %v, want upstream message", err)
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %v/%v, want %d", statusErr, ok, http.StatusTooManyRequests)
	}
}

func TestResponsesStreamCompletionTrackerReturnsResponseFailedAfterPriorEvents(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: response.created\ndata: {\"type\":\"response.created\"}\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\nevent: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"upstream failed after delta\"}}}"))

	err := tracker.ErrIfIncomplete()
	if err == nil {
		t.Fatal("ErrIfIncomplete() error = nil, want response.failed error")
	}
	if strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("ErrIfIncomplete() error = %v, should preserve upstream failure instead of incomplete stream", err)
	}
	if !strings.Contains(err.Error(), "upstream failed after delta") {
		t.Fatalf("ErrIfIncomplete() error = %v, want upstream message", err)
	}
}

func TestResponsesStreamCompletionTrackerReturnsTopLevelError(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: error\ndata: {\"type\":\"error\",\"code\":\"internal_server_error\",\"message\":\"upstream exploded\"}"))

	err := tracker.ErrIfIncomplete()
	if err == nil {
		t.Fatal("ErrIfIncomplete() error = nil, want top-level error")
	}
	if strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("ErrIfIncomplete() error = %v, should preserve upstream error instead of incomplete stream", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("ErrIfIncomplete() error = %v, want upstream message", err)
	}
}

func TestResponsesStreamCompletionTrackerRejectsTrueIncompleteStream(t *testing.T) {
	t.Parallel()

	tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
	tracker.Observe([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}"))

	err := tracker.ErrIfIncomplete()
	if err == nil {
		t.Fatal("ErrIfIncomplete() error = nil, want incomplete stream error")
	}
	if !strings.Contains(err.Error(), "response.completed") {
		t.Fatalf("ErrIfIncomplete() error = %v, want incomplete stream message", err)
	}
}

// A reply the upstream cut short ends with response.incomplete; older peers call
// the completed event response.done. Either way the stream is whole and must not
// be reported as broken.
func TestResponsesStreamCompletionTrackerAcceptsIncompleteAndDone(t *testing.T) {
	t.Parallel()

	for _, chunk := range []string{
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}",
		"data: {\"type\":\"response.incomplete\"}",
		"event: response.done\ndata: {\"type\":\"response.done\"}",
	} {
		tracker := newResponsesStreamCompletionTracker(sdktranslator.FormatOpenAIResponse)
		tracker.Observe([]byte("data: {\"type\":\"response.created\"}\n\n"))
		tracker.Observe([]byte(chunk))
		if err := tracker.ErrIfIncomplete(); err != nil {
			t.Fatalf("ErrIfIncomplete() after %q = %v, want nil", chunk, err)
		}
	}
}

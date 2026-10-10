package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// The shapes below are what gemini-3.8-flash-high actually sends: a thought, then
// a final chunk with an empty text part and MALFORMED_FUNCTION_CALL.
const (
	sseThought   = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"I will check the remotes next.","thought":true}]}}],"responseId":"r1"}}` + "\n\n"
	sseMalformed = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"sig-malformed","text":""}]},"finishReason":"MALFORMED_FUNCTION_CALL","finishMessage":"Malformed function call: Failed to parse function call: Function call is empty - no input to parse."}],"usageMetadata":{"promptTokenCount":100,"totalTokenCount":160,"thoughtsTokenCount":60},"responseId":"r1"}}` + "\n\n"
	sseCall      = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"sig-call","functionCall":{"name":"bash","args":{"command":"git branch -r"}}}]}}],"responseId":"r2"}}` + "\n\n"
	sseStop      = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":12,"totalTokenCount":200,"thoughtsTokenCount":88},"responseId":"r2"}}` + "\n\n"
)

const malformedCallResponsesRequest = `{"model":"gemini-3.8-flash-high","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"check the remotes"}]}],"tools":[{"type":"function","name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`

// upstreamSequence serves the given SSE bodies to successive requests; once
// exhausted it keeps serving the last one.
func upstreamSequence(t *testing.T, bodies ...string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&calls, 1)) - 1
		if n >= len(bodies) {
			n = len(bodies) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(bodies[n]))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func runMalformedCallStream(t *testing.T, baseURL string) []string {
	t.Helper()
	result, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(context.Background(), antigravityTestAuth(baseURL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: []byte(malformedCallResponsesRequest),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var events []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		for _, line := range strings.Split(string(chunk.Payload), "\n") {
			if strings.HasPrefix(line, "data:") {
				events = append(events, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	return events
}

func eventsOfType(events []string, eventType string) []string {
	var matched []string
	for _, event := range events {
		if gjson.Get(event, "type").String() == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// The answer that ends in a malformed call is replaced by a retry on the same
// client response: one terminal event, carrying the function call.
func TestAntigravityStreamRetriesMalformedFunctionCall(t *testing.T) {
	server, calls := upstreamSequence(t, sseThought+sseMalformed, sseThought+sseCall+sseStop)
	events := runMalformedCallStream(t, server.URL)

	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if n := len(eventsOfType(events, "response.created")); n != 1 {
		t.Fatalf("response.created events = %d, want 1 (the retry must continue the same response)", n)
	}
	completed := eventsOfType(events, "response.completed")
	if len(completed) != 1 || len(eventsOfType(events, "response.failed")) != 0 {
		t.Fatalf("terminal events: completed=%d failed=%d, want exactly one completed", len(completed), len(eventsOfType(events, "response.failed")))
	}
	var calledBash bool
	gjson.Get(completed[0], "response.output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == "bash" {
			calledBash = true
		}
		return true
	})
	if !calledBash {
		t.Fatalf("completed output has no bash function_call: %s", gjson.Get(completed[0], "response.output").Raw)
	}
	for _, event := range events {
		if strings.Contains(event, "MALFORMED_FUNCTION_CALL") {
			t.Fatalf("the retried malformed finish leaked to the client: %s", event)
		}
	}
}

// Retries are bounded. Once they are spent the upstream's own end of turn is
// delivered, so the client response still terminates exactly once (how that
// terminal reads is the translator's decision).
func TestAntigravityStreamMalformedFunctionCallStopsAfterRetries(t *testing.T) {
	server, calls := upstreamSequence(t, sseThought+sseMalformed)
	events := runMalformedCallStream(t, server.URL)

	if got, want := atomic.LoadInt32(calls), int32(antigravityMalformedCallRetries+1); got != want {
		t.Fatalf("upstream calls = %d, want %d", got, want)
	}
	terminals := len(eventsOfType(events, "response.completed")) + len(eventsOfType(events, "response.failed"))
	if terminals != 1 {
		t.Fatalf("terminal events = %d, want 1", terminals)
	}
	if n := len(eventsOfType(events, "response.created")); n != 1 {
		t.Fatalf("response.created events = %d, want 1", n)
	}
}

// Once the client has text, a retry would duplicate it; the answer stands.
func TestAntigravityStreamDoesNotRetryAfterVisibleOutput(t *testing.T) {
	sseText := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"Checking now."}]}}],"responseId":"r1"}}` + "\n\n"
	server, calls := upstreamSequence(t, sseText+sseMalformed, sseCall+sseStop)
	events := runMalformedCallStream(t, server.URL)

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if n := len(eventsOfType(events, "response.completed")); n != 1 {
		t.Fatalf("response.completed events = %d, want 1", n)
	}
}

// The non-streaming path assembles the answer before replying, so a malformed
// answer is discarded and asked again.
func TestAntigravityNonStreamRetriesMalformedFunctionCall(t *testing.T) {
	server, calls := upstreamSequence(t, sseThought+sseMalformed, sseThought+sseCall+sseStop)
	resp, err := NewAntigravityExecutor(&config.Config{}).Execute(context.Background(), antigravityTestAuth(server.URL), cliproxyexecutor.Request{
		Model:   "gemini-3.8-flash-high",
		Payload: []byte(strings.Replace(malformedCallResponsesRequest, `"stream":true`, `"stream":false`, 1)),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if status := gjson.GetBytes(resp.Payload, "status").String(); status != "completed" {
		t.Fatalf("status = %q, want completed: %s", status, resp.Payload)
	}
	if !strings.Contains(gjson.GetBytes(resp.Payload, "output").Raw, `"function_call"`) {
		t.Fatalf("output has no function_call: %s", resp.Payload)
	}
}

func TestGeminiMalformedCallGuard(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"thought then malformed", []string{sseThought, sseMalformed}, true},
		{"unwrapped payload", []string{`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`}, true},
		{"normal stop", []string{sseThought, sseStop}, false},
		{"text before malformed", []string{`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, sseMalformed}, false},
		{"call before malformed", []string{sseCall, sseMalformed}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var guard geminiMalformedCallGuard
			var got bool
			for _, chunk := range tc.chunks {
				got = guard.observe(jsonPayload([]byte(strings.TrimSpace(chunk))))
			}
			if got != tc.want {
				t.Fatalf("observe() = %v, want %v", got, tc.want)
			}
		})
	}
}

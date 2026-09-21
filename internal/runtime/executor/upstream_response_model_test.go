package executor

import "testing"

func TestUpstreamResponseModelObserverProtocols(t *testing.T) {
	cases := []struct {
		name     string
		payloads []string
		want     string
	}{
		{
			name:     "anthropic non-streaming body",
			payloads: []string{`{"type":"message","model":"claude-sonnet-4-5","usage":{"input_tokens":3}}`},
			want:     "claude-sonnet-4-5",
		},
		{
			name:     "anthropic message_start",
			payloads: []string{`data: {"type":"message_start","message":{"model":"claude-opus-4-6","usage":{"input_tokens":1}}}`},
			want:     "claude-opus-4-6",
		},
		{
			name:     "openai chat completion chunk",
			payloads: []string{`data: {"id":"chatcmpl-1","model":"gpt-5.4","choices":[]}`},
			want:     "gpt-5.4",
		},
		{
			name: "openai responses terminal event wins over created",
			payloads: []string{
				`data: {"type":"response.created","response":{"model":"gpt-5.4"}}`,
				`data: {"type":"response.completed","response":{"model":"gpt-5.4-2026-03-01"}}`,
			},
			want: "gpt-5.4-2026-03-01",
		},
		{
			name: "gemini keeps the latest modelVersion",
			payloads: []string{
				`data: {"modelVersion":"gemini-3.8-flash","candidates":[]}`,
				`data: {"modelVersion":"gemini-3.8-flash-exp-a","candidates":[]}`,
			},
			want: "gemini-3.8-flash-exp-a",
		},
		{
			name:     "antigravity nested response envelope",
			payloads: []string{`data: {"response":{"modelVersion":"gemini-3.8-pro","candidates":[]}}`},
			want:     "gemini-3.8-pro",
		},
		{
			name:     "clinepass style provider envelope",
			payloads: []string{`{"success":true,"data":{"model":"cline-pass/qwen3.7-max","usage":{"total_tokens":1}}}`},
			want:     "cline-pass/qwen3.7-max",
		},
		{
			name: "first non-terminal declaration is kept",
			payloads: []string{
				`data: {"id":"chatcmpl-1","model":"gpt-5.4","choices":[]}`,
				`data: {"id":"chatcmpl-1","model":"gpt-5.4-mini","choices":[]}`,
			},
			want: "gpt-5.4",
		},
		{
			name: "content deltas without a model do not clear the observation",
			payloads: []string{
				`data: {"type":"message_start","message":{"model":"claude-opus-4-6"}}`,
				`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
				`data: [DONE]`,
			},
			want: "claude-opus-4-6",
		},
		{
			name:     "sse event lines are ignored",
			payloads: []string{"event: response.completed"},
			want:     "",
		},
		{
			name:     "malformed json declaring a model is rejected",
			payloads: []string{`{"model":"ghost-model","truncated":`},
			want:     "",
		},
		{
			name:     "non-string model is ignored",
			payloads: []string{`{"model":123,"usage":{}}`},
			want:     "",
		},
		{
			name:     "empty model is ignored",
			payloads: []string{`{"model":"   "}`},
			want:     "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var observer upstreamResponseModelObserver
			for _, payload := range tc.payloads {
				observer.observe([]byte(payload))
			}
			if got := observer.model(); got != tc.want {
				t.Fatalf("model() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUpstreamResponseModelObserverTruncatesHostileName(t *testing.T) {
	long := make([]byte, 0, 512)
	for i := 0; i < 500; i++ {
		long = append(long, 'a')
	}
	var observer upstreamResponseModelObserver
	observer.observe([]byte(`{"model":"` + string(long) + `"}`))
	if got := len([]rune(observer.model())); got != upstreamResponseModelMaxLength {
		t.Fatalf("observed name length = %d, want %d", got, upstreamResponseModelMaxLength)
	}
}

func TestUpstreamResponseModelObserverNilSafe(t *testing.T) {
	var observer *upstreamResponseModelObserver
	observer.observe([]byte(`{"model":"gpt-5.4"}`))
	if got := observer.model(); got != "" {
		t.Fatalf("nil observer model() = %q, want empty", got)
	}
}

// The reporter must audit the upstream even when request-body storage is off:
// that toggle governs what content is retained, not whether the row is audited.
func TestUsageReporterObservesWithContentCaptureDisabled(t *testing.T) {
	reporter := &usageReporter{captureFullContent: false}
	reporter.compactOutputFull.Store(true)
	reporter.appendOutputChunk([]byte(`data: {"modelVersion":"gemini-3.8-flash-exp-a"}`))
	if got := reporter.upstreamResponse.model(); got != "gemini-3.8-flash-exp-a" {
		t.Fatalf("observed model = %q, want gemini-3.8-flash-exp-a", got)
	}
}

func TestUsageReporterObservesNonStreamingBody(t *testing.T) {
	reporter := &usageReporter{}
	reporter.observeUpstreamResponse([]byte(`{"type":"message","model":"claude-sonnet-4-5"}`))
	if got := reporter.upstreamResponse.model(); got != "claude-sonnet-4-5" {
		t.Fatalf("observed model = %q, want claude-sonnet-4-5", got)
	}
}

package observability

import (
	"context"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestObserveUsageCountsAttemptsByOutcome(t *testing.T) {
	e := NewExporter()
	ok := coreusage.Record{Provider: "codex", Model: "gpt-5.5", AuthIndex: "a1b2", LatencyMs: 1500, FirstTokenMs: 400, Streaming: true}
	e.ObserveUsage(ok)
	e.ObserveUsage(ok)
	e.ObserveUsage(coreusage.Record{Provider: "codex", Model: "gpt-5.5", AuthIndex: "a1b2", LatencyMs: 20, Failed: true})

	if got := value(t, e, "clirelay_upstream_requests_total", map[string]string{"provider": "codex", "model": "gpt-5.5", "outcome": "success"}).value; got != 2 {
		t.Fatalf("success attempts = %v, want 2", got)
	}
	if got := value(t, e, "clirelay_upstream_requests_total", map[string]string{"outcome": "failure"}).value; got != 1 {
		t.Fatalf("failed attempts = %v, want 1", got)
	}
	if got := value(t, e, "clirelay_upstream_account_requests_total", map[string]string{"provider": "codex", "auth_index": "a1b2", "outcome": "failure"}).value; got != 1 {
		t.Fatalf("per-account failures = %v, want 1", got)
	}

	// The failed attempt's 20 ms must not pull the latency of answers down.
	duration := value(t, e, "clirelay_upstream_request_duration_seconds", map[string]string{"provider": "codex", "model": "gpt-5.5"})
	if duration.value != 2 || duration.sum != 3 {
		t.Fatalf("duration count/sum = %v/%v, want 2/3 (successes only)", duration.value, duration.sum)
	}
	firstToken := value(t, e, "clirelay_upstream_first_token_seconds", map[string]string{"provider": "codex"})
	if firstToken.value != 2 || firstToken.sum != 0.8 {
		t.Fatalf("first token count/sum = %v/%v, want 2/0.8", firstToken.value, firstToken.sum)
	}
}

func TestObserveUsageFirstTokenOnlyForStreams(t *testing.T) {
	e := NewExporter()
	// A non-streamed answer arrives whole: its "first token" is its duration.
	e.ObserveUsage(coreusage.Record{Provider: "claude", Model: "claude-opus-5-5", LatencyMs: 9000, FirstTokenMs: 9000})
	absent(t, e, "clirelay_upstream_first_token_seconds", map[string]string{"provider": "claude"})
	if got := value(t, e, "clirelay_upstream_request_duration_seconds", map[string]string{"provider": "claude"}).value; got != 1 {
		t.Fatalf("duration count = %v, want 1", got)
	}
}

func TestObserveUsageSkipsAccountSeriesWithoutIndex(t *testing.T) {
	e := NewExporter()
	e.ObserveUsage(coreusage.Record{Provider: "gemini", Model: "gemini-3-pro"})
	if got := gather(t, e, "clirelay_upstream_account_requests_total"); len(got) != 0 {
		t.Fatalf("account series without an auth index: %v", describe(got))
	}
	value(t, e, "clirelay_upstream_requests_total", map[string]string{"provider": "gemini", "model": "gemini-3-pro", "outcome": "success"})
}

func TestTokenBreakdownAddsUpToThePrompt(t *testing.T) {
	cases := []struct {
		name   string
		detail coreusage.Detail
		want   map[string]int64
	}{
		{
			// OpenAI, Codex, Gemini: prompt_tokens includes what was read from cache.
			name:   "cache read inside input",
			detail: coreusage.Detail{InputTokens: 1000, CacheReadTokens: 800, CacheReadIncludedInInput: true, OutputTokens: 50, ReasoningTokens: 20},
			want:   map[string]int64{"input": 200, "cache_read": 800, "cache_write": 0, "output": 50, "reasoning": 20},
		},
		{
			// Anthropic: input_tokens excludes both reads and writes.
			name:   "anthropic style",
			detail: coreusage.Detail{InputTokens: 30, CacheReadTokens: 900, CacheWriteTokens: 70, OutputTokens: 10},
			want:   map[string]int64{"input": 30, "cache_read": 900, "cache_write": 70, "output": 10, "reasoning": 0},
		},
		{
			// An upstream claiming to have read more than the whole prompt:
			// the read count is trusted, as billing and request logs do.
			name:   "read larger than input",
			detail: coreusage.Detail{InputTokens: 100, CacheReadTokens: 150, CacheReadIncludedInInput: true},
			want:   map[string]int64{"input": 0, "cache_read": 150, "cache_write": 0, "output": 0, "reasoning": 0},
		},
		{
			name:   "negative values are ignored",
			detail: coreusage.Detail{InputTokens: -5, OutputTokens: -1, CacheWriteTokens: -3},
			want:   map[string]int64{"input": 0, "cache_read": 0, "cache_write": 0, "output": 0, "reasoning": 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenBreakdown(tc.detail)
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s = %d, want %d (all %v)", key, got[key], want, got)
				}
			}
		})
	}
}

func TestObserveUsageExportsTokensByType(t *testing.T) {
	e := NewExporter()
	e.ObserveUsage(coreusage.Record{Provider: "codex", Model: "gpt-5.5", Detail: coreusage.Detail{
		InputTokens: 1000, CacheReadTokens: 800, CacheReadIncludedInInput: true, OutputTokens: 40,
	}})
	for tokenType, want := range map[string]float64{"input": 200, "cache_read": 800, "output": 40} {
		if got := value(t, e, "clirelay_upstream_tokens_total", map[string]string{"type": tokenType}).value; got != want {
			t.Fatalf("tokens{type=%s} = %v, want %v", tokenType, got, want)
		}
	}
	// Zero counts create no series: a provider that never reports cache
	// writes does not grow a flat-zero line per model.
	absent(t, e, "clirelay_upstream_tokens_total", map[string]string{"type": "cache_write"})
}

func TestModelLabelOverflowsPastTheCap(t *testing.T) {
	e := NewExporter()
	e.models = newLabelGuard(2)
	for _, model := range []string{"m1", "m2", "m3", "m4"} {
		e.ObserveUsage(coreusage.Record{Provider: "openai-compatibility", Model: model})
	}
	// The pair already admitted keeps its own series.
	e.ObserveUsage(coreusage.Record{Provider: "openai-compatibility", Model: "m1"})

	if got := value(t, e, "clirelay_upstream_requests_total", map[string]string{"model": "m1"}).value; got != 2 {
		t.Fatalf("m1 = %v, want 2", got)
	}
	if got := value(t, e, "clirelay_upstream_requests_total", map[string]string{"model": overflowLabel}).value; got != 2 {
		t.Fatalf("overflow = %v, want 2 (m3 and m4)", got)
	}
	absent(t, e, "clirelay_upstream_requests_total", map[string]string{"model": "m3"})
}

func TestAccountLabelOverflowsPastTheCap(t *testing.T) {
	e := NewExporter()
	e.accounts = newLabelGuard(1)
	e.ObserveUsage(coreusage.Record{Provider: "codex", Model: "gpt-5.5", AuthIndex: "first"})
	e.ObserveUsage(coreusage.Record{Provider: "codex", Model: "gpt-5.5", AuthIndex: "second"})
	value(t, e, "clirelay_upstream_account_requests_total", map[string]string{"auth_index": "first"})
	value(t, e, "clirelay_upstream_account_requests_total", map[string]string{"auth_index": overflowLabel})
}

func TestLabelValueSanitises(t *testing.T) {
	if got := labelValue("  ", "unknown"); got != "unknown" {
		t.Fatalf("blank = %q", got)
	}
	if got := labelValue("bad\xffname", "x"); got != "bad�name" {
		t.Fatalf("invalid utf-8 = %q", got)
	}
	long := strings.Repeat("模", 100) // 300 bytes
	got := labelValue(long, "x")
	if len(got) > maxLabelLength || !strings.HasPrefix(long, got) || got == "" {
		t.Fatalf("long value cut to %d bytes, prefix=%v", len(got), strings.HasPrefix(long, got))
	}
	// The registry rejects label values that are not valid UTF-8; a raw one
	// must still be recorded rather than panic.
	e := NewExporter()
	e.ObserveUsage(coreusage.Record{Provider: "x\xfe", Model: long})
	value(t, e, "clirelay_upstream_requests_total", map[string]string{"provider": "x�"})
}

func TestObserveHTTPNormalisesLabels(t *testing.T) {
	e := NewExporter()
	e.ObserveHTTP("post", "/v1/chat/completions", 200, 2*time.Second)
	e.ObserveHTTP("BREW", "", 404, time.Millisecond)

	if got := value(t, e, "clirelay_http_requests_total", map[string]string{"method": "POST", "route": "/v1/chat/completions", "status_code": "200"}).value; got != 1 {
		t.Fatalf("chat completions = %v", got)
	}
	// An invented verb and an unmatched path stay two fixed label values.
	value(t, e, "clirelay_http_requests_total", map[string]string{"method": "OTHER", "route": "unmatched", "status_code": "404"})
	if got := value(t, e, "clirelay_http_request_duration_seconds", map[string]string{"method": "POST"}).sum; got != 2 {
		t.Fatalf("duration sum = %v, want 2", got)
	}
}

func TestUsagePluginForwardsRecords(t *testing.T) {
	e := NewExporter()
	usagePlugin{exporter: e}.HandleUsage(context.Background(), coreusage.Record{Provider: "kimi", Model: "kimi-k3"})
	value(t, e, "clirelay_upstream_requests_total", map[string]string{"provider": "kimi"})
}

package observability

import (
	"context"
	"sync"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

// ObserveUsage records one upstream request from its usage record. The usage
// manager hands records to plugins one at a time, so this must not block: it
// only updates in-memory counters.
func (e *Exporter) ObserveUsage(record coreusage.Record) {
	if e == nil {
		return
	}
	provider := labelValue(record.Provider, unknownLabel)
	model := labelValue(record.Model, unknownLabel)
	if !e.models.admit(provider + "\x00" + model) {
		model = overflowLabel
	}
	outcome := "success"
	if record.Failed {
		outcome = "failure"
	}

	e.upstreamRequests.WithLabelValues(provider, model, outcome).Inc()
	if !record.Failed {
		// A failed attempt's duration says how fast it failed, which would
		// drag the latency of real answers towards zero; failures are
		// counted above instead.
		if record.LatencyMs > 0 {
			e.upstreamDuration.WithLabelValues(provider, model).Observe(float64(record.LatencyMs) / 1000)
		}
		// Non-streamed responses arrive whole, so their "first token" is the
		// full duration again.
		if record.Streaming && record.FirstTokenMs > 0 {
			e.upstreamFirstToken.WithLabelValues(provider, model).Observe(float64(record.FirstTokenMs) / 1000)
		}
	}
	for tokenType, count := range tokenBreakdown(record.Detail) {
		if count > 0 {
			e.upstreamTokens.WithLabelValues(provider, model, tokenType).Add(float64(count))
		}
	}

	if index := labelValue(record.AuthIndex, ""); index != "" {
		if !e.accounts.admit(index) {
			index = overflowLabel
		}
		e.accountRequests.WithLabelValues(provider, index, outcome).Inc()
	}
}

// tokenBreakdown splits a record's tokens so the prompt types add up to the
// prompt the model processed, the way request logs size it: upstreams that
// count cache reads inside their input figure (OpenAI, Codex, Gemini) have them
// taken out, Anthropic's figure already excludes both reads and writes.
func tokenBreakdown(detail coreusage.Detail) map[string]int64 {
	input := max(detail.InputTokens, 0)
	read := max(detail.CacheReadTokens, 0)
	if detail.CacheReadIncludedInInput {
		input = max(input-read, 0)
	}
	return map[string]int64{
		"input":       input,
		"cache_read":  read,
		"cache_write": max(detail.CacheWriteTokens, 0),
		"output":      max(detail.OutputTokens, 0),
		"reasoning":   max(detail.ReasoningTokens, 0),
	}
}

type usagePlugin struct{ exporter *Exporter }

func (p usagePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	p.exporter.ObserveUsage(record)
}

var registerUsageOnce sync.Once

// RegisterUsagePlugin subscribes the process-wide exporter to usage records.
// Calling it more than once has no further effect.
func RegisterUsagePlugin() {
	registerUsageOnce.Do(func() {
		coreusage.RegisterPlugin(usagePlugin{exporter: Default()})
	})
}

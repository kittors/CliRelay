package usage

import (
	"database/sql"
	"strconv"
)

// Generation speed: how fast a model streams its answer once it has started.
//
// Per request it is output tokens over the time after the first token,
// latency_ms - first_token_ms. The first-token wait (queueing, prompt
// processing and, on thinking models, the thinking itself) stays out of it. It
// used to be in: dividing by the whole duration reported under 20 tokens per
// second for a model whose requests stream at about 150 once they start,
// because nine tenths of its time is spent before the first token.
//
// A request is a sample only when it
//   - succeeded, was streamed and recorded its first token. A reply that is not
//     streamed arrives in one piece, and first_token_ms is 0 when nothing was
//     recorded, which would put the wait back into the measured time;
//   - generated for at least generationMinMs and returned at least
//     generationMinOutputTokens output tokens. Below either, the timing of a
//     few network writes decides the ratio rather than the model.
//
// Every other request is left out of both sides of the ratio.
//
// Reasoning tokens are not counted. Gemini reports its thinking apart from the
// output and spends it before the first token is written, outside the measured
// time; OpenAI-style upstreams already count reasoning inside output_tokens, so
// adding reasoning_tokens counted it twice there.
const (
	generationMinMs           = 500
	generationMinOutputTokens = 50
)

// requestLogGenerationSampleSQL selects the request_logs rows that are
// generation-speed samples. It must say what generationSample says.
var requestLogGenerationSampleSQL = ` AND failed = 0 AND streaming = 1 AND first_token_ms > 0` +
	` AND output_tokens >= ` + strconv.Itoa(generationMinOutputTokens) +
	` AND latency_ms - first_token_ms >= ` + strconv.Itoa(generationMinMs)

// generationSample returns a request's time after its first token, and whether
// the request is a generation-speed sample at all.
func generationSample(failed, streaming bool, latencyMs, firstTokenMs, outputTokens int64) (int64, bool) {
	if failed || !streaming || firstTokenMs <= 0 || outputTokens < generationMinOutputTokens {
		return 0, false
	}
	generationMs := latencyMs - firstTokenMs
	if generationMs < generationMinMs {
		return 0, false
	}
	return generationMs, true
}

// GenerationUsage is the generation-speed side of an aggregate. Its fields sit
// at the top level of MonitorTotals.
type GenerationUsage struct {
	// GenerationSamples counts the requests that measured generation speed.
	GenerationSamples int64 `json:"generation_samples"`
	// GenerationTokensPerSecond is the mean of those requests' own speeds, and
	// null without a sample. Buckets written before the generation columns
	// existed hold no sample, so a window made only of them has no speed rather
	// than a speed of zero.
	//
	// It is the mean per request, not total output over total time: that ratio
	// weighs each request by its length, and a few long replies that stall
	// between chunks pull it far below the typical request (on production data
	// 94 tokens per second for a model whose requests have a median of 146).
	GenerationTokensPerSecond *float64 `json:"generation_tokens_per_second"`
	// GenerationOutputTokens and GenerationMs sum the samples' output tokens and
	// their time after the first token.
	GenerationOutputTokens int64 `json:"generation_output_tokens"`
	GenerationMs           int64 `json:"generation_ms"`
}

// legacyOutputTokensPerSecond is what the deprecated output_tokens_per_second
// field reports: the generation speed, or 0 without a sample, because older
// panels read it as a number and show a dash for 0.
func (g GenerationUsage) legacyOutputTokensPerSecond() float64 {
	if g.GenerationTokensPerSecond == nil {
		return 0
	}
	return *g.GenerationTokensPerSecond
}

// generationTotals accumulates the generation columns of usage_rollup_buckets.
type generationTotals struct {
	Count        int64
	OutputTokens int64
	SumMs        int64
	// SpeedSum adds up each sample's own tokens per second; the mean speed is
	// SpeedSum / Count.
	SpeedSum float64
}

func (g *generationTotals) add(o generationTotals) {
	g.Count += o.Count
	g.OutputTokens += o.OutputTokens
	g.SumMs += o.SumMs
	g.SpeedSum += o.SpeedSum
}

func (g generationTotals) usage() GenerationUsage {
	out := GenerationUsage{
		GenerationSamples:      g.Count,
		GenerationOutputTokens: g.OutputTokens,
		GenerationMs:           g.SumMs,
	}
	if g.Count > 0 {
		speed := g.SpeedSum / float64(g.Count)
		out.GenerationTokensPerSecond = &speed
	}
	return out
}

func (g *generationTotals) scanTargets() []any {
	return []any{&g.Count, &g.OutputTokens, &g.SumMs, &g.SpeedSum}
}

// requestGenerationTotals is one request's contribution: one sample, or nothing
// when the request does not measure generation speed.
func requestGenerationTotals(failed, streaming bool, latencyMs, firstTokenMs, outputTokens int64) generationTotals {
	generationMs, ok := generationSample(failed, streaming, latencyMs, firstTokenMs, outputTokens)
	if !ok {
		return generationTotals{}
	}
	return generationTotals{
		Count:        1,
		OutputTokens: outputTokens,
		SumMs:        generationMs,
		SpeedSum:     float64(outputTokens) * 1000 / float64(generationMs),
	}
}

// rollupGenerationSumsSQL aggregates the generation columns of
// usage_rollup_buckets, in the order of generationTotals.
const rollupGenerationSumsSQL = `
	COALESCE(SUM(generation_count), 0),
	COALESCE(SUM(generation_output_tokens), 0),
	COALESCE(SUM(generation_sum_ms), 0),
	COALESCE(SUM(generation_speed_sum), 0)`

// migrateUsageRollupGenerationColumns adds the generation columns to a SQLite
// usage_rollup_buckets table. Buckets filled before them keep zeros there,
// which reads as "no sample". PostgreSQL gets them from the runtime migration
// 202611040001_usage_rollup_generation_speed and is skipped here, for the
// reason given at migrateUsageRollupCacheColumns.
func migrateUsageRollupGenerationColumns(db *sql.DB) {
	if metadataForUsageDB(db).driver == "postgres" {
		return
	}
	addSQLiteColumns(db, "usage_rollup_buckets", []string{
		"generation_count INTEGER NOT NULL DEFAULT 0",
		"generation_output_tokens INTEGER NOT NULL DEFAULT 0",
		"generation_sum_ms INTEGER NOT NULL DEFAULT 0",
		"generation_speed_sum REAL NOT NULL DEFAULT 0",
	})
}

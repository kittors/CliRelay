package executor

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// antigravityClaudeMaxOutputTokens is the ceiling CloudCode puts on
// generationConfig.maxOutputTokens for the Claude family. Probed against the
// live upstream on 2026-09-12: 64000 answers 200 and 64001 answers a bare 400
// INVALID_ARGUMENT naming no field. The Gemini family takes 65536 at the same
// spot, which is why only Claude requests failed.
const antigravityClaudeMaxOutputTokens = 64000

// clampAntigravityOutputTokens caps maxOutputTokens at what the model family
// accepts, and keeps thinkingBudget under the cap it lands on.
//
// Clients pick their own ceiling and agent clients ask for the largest one they
// know of — 65536 is common — so the value went upstream unchanged and failed
// every Claude request before a single token was generated. The failure names
// no field, so it reads as "the model is broken" rather than as one number
// being one too high.
//
// Capping here rather than in a translator covers all four entrypoint formats
// at once: this is the last point they share. Only requests that were already
// doomed change shape; anything at or below the cap is passed through untouched.
func clampAntigravityOutputTokens(payload string, modelName string) string {
	if cache.GetModelGroup(modelName) != "claude" {
		return payload
	}

	const maxOutputTokensPath = "request.generationConfig.maxOutputTokens"
	current := gjson.Get(payload, maxOutputTokensPath)
	if !current.Exists() || current.Int() <= antigravityClaudeMaxOutputTokens {
		return payload
	}

	updated, err := sjson.Set(payload, maxOutputTokensPath, antigravityClaudeMaxOutputTokens)
	if err != nil {
		return payload
	}

	// The upstream also requires max_tokens > thinking.budget_tokens, and
	// answers a violation with `max_tokens` must be greater than
	// `thinking.budget_tokens`. A budget that cleared the client's own ceiling
	// can exceed the one we just imposed, so lower it to stay inside the cap
	// rather than trade one 400 for another.
	const thinkingBudgetPath = "request.generationConfig.thinkingConfig.thinkingBudget"
	budget := gjson.Get(updated, thinkingBudgetPath)
	if !budget.Exists() || budget.Int() < antigravityClaudeMaxOutputTokens {
		return updated
	}
	withBudget, err := sjson.Set(updated, thinkingBudgetPath, antigravityClaudeMaxOutputTokens-1)
	if err != nil {
		return updated
	}
	return withBudget
}

package responses

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

// Tool calls that arrive without a call_id get a random toolu_ id.
var maskRandomIDs = translatortest.MaskRandomIDs("toolu_")

func TestConvertOpenAIResponsesRequestToClaude_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ResponsesRequest,
		Models:   []string{"claude-sonnet-4-5", "claude-opus-4-1", "claude-3-5-haiku-20241022"},
		Mask:     maskRandomIDs,
	}, ConvertOpenAIResponsesRequestToClaude, convertOpenAIResponsesRequestToClaudeReference)
}

func TestConvertOpenAIResponsesRequestToClaude_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.CodexResponsesSession, "claude-sonnet-4-5", maskRandomIDs, ConvertOpenAIResponsesRequestToClaude, convertOpenAIResponsesRequestToClaudeReference)
}

// Appending every message to the whole request, and every part of a thinking
// turn to the last message through the whole request, made the cost grow with
// the square of the session length.
func TestConvertOpenAIResponsesRequestToClaude_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.CodexResponsesSession, "claude-sonnet-4-5", ConvertOpenAIResponsesRequestToClaude)
}

func BenchmarkConvertOpenAIResponsesRequestToClaude_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexResponsesSession, "claude-sonnet-4-5", ConvertOpenAIResponsesRequestToClaude)
}

func BenchmarkConvertOpenAIResponsesRequestToClaude_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexResponsesSession, "claude-sonnet-4-5", convertOpenAIResponsesRequestToClaudeReference)
}

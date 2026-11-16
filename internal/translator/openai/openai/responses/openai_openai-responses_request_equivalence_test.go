package responses

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ResponsesRequest,
		Models:   []string{"deepseek-chat", "kimi-k2"},
	}, ConvertOpenAIResponsesRequestToOpenAIChatCompletions, convertOpenAIResponsesRequestToOpenAIChatCompletionsReference)
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.CodexResponsesSession, "deepseek-chat", nil, ConvertOpenAIResponsesRequestToOpenAIChatCompletions, convertOpenAIResponsesRequestToOpenAIChatCompletionsReference)
}

// Appending every message to the whole request made the cost grow with the
// square of the session length.
func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.CodexResponsesSession, "deepseek-chat", ConvertOpenAIResponsesRequestToOpenAIChatCompletions)
}

func BenchmarkConvertOpenAIResponsesRequestToOpenAIChatCompletions_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexResponsesSession, "deepseek-chat", ConvertOpenAIResponsesRequestToOpenAIChatCompletions)
}

func BenchmarkConvertOpenAIResponsesRequestToOpenAIChatCompletions_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexResponsesSession, "deepseek-chat", convertOpenAIResponsesRequestToOpenAIChatCompletionsReference)
}

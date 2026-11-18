package chat_completions

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertOpenAIRequestToAntigravity_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ChatRequest,
		Models:   []string{"gemini-3-pro-preview", "claude-sonnet-4-5", "gemini-2.5-flash"},
	}, ConvertOpenAIRequestToAntigravity, convertOpenAIRequestToAntigravityReference)
}

func TestConvertOpenAIRequestToAntigravity_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.CodexChatSession, "gemini-3-pro-preview", nil, ConvertOpenAIRequestToAntigravity, convertOpenAIRequestToAntigravityReference)
}

// Appending every content to the whole request made the cost grow with the
// square of the session length.
func TestConvertOpenAIRequestToAntigravity_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.CodexChatSession, "gemini-3-pro-preview", ConvertOpenAIRequestToAntigravity)
}

func BenchmarkConvertOpenAIRequestToAntigravity_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexChatSession, "gemini-3-pro-preview", ConvertOpenAIRequestToAntigravity)
}

func BenchmarkConvertOpenAIRequestToAntigravity_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexChatSession, "gemini-3-pro-preview", convertOpenAIRequestToAntigravityReference)
}

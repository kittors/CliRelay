package chat_completions

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertOpenAIRequestToGeminiCLI_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ChatRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview", "gemini-2.0-flash"},
	}, ConvertOpenAIRequestToGeminiCLI, convertOpenAIRequestToGeminiCLIReference)
}

func TestConvertOpenAIRequestToGeminiCLI_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.CodexChatSession, "gemini-2.5-pro", nil, ConvertOpenAIRequestToGeminiCLI, convertOpenAIRequestToGeminiCLIReference)
}

// Appending every content to the whole request made the cost grow with the
// square of the session length.
func TestConvertOpenAIRequestToGeminiCLI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.CodexChatSession, "gemini-2.5-pro", ConvertOpenAIRequestToGeminiCLI)
}

func BenchmarkConvertOpenAIRequestToGeminiCLI_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexChatSession, "gemini-2.5-pro", ConvertOpenAIRequestToGeminiCLI)
}

func BenchmarkConvertOpenAIRequestToGeminiCLI_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.CodexChatSession, "gemini-2.5-pro", convertOpenAIRequestToGeminiCLIReference)
}

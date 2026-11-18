package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

// Function calls get a random call_ id: Gemini has none to pair them by.
var maskRandomIDs = translatortest.MaskRandomIDs("call_")

func TestConvertGeminiRequestToOpenAI_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiRequest,
		Models:   []string{"deepseek-chat", "kimi-k2"},
		Mask:     maskRandomIDs,
	}, ConvertGeminiRequestToOpenAI, convertGeminiRequestToOpenAIReference)
}

func TestConvertGeminiRequestToOpenAI_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiSession, "deepseek-chat", maskRandomIDs, ConvertGeminiRequestToOpenAI, convertGeminiRequestToOpenAIReference)
}

// Appending every message, and then every tool, to the whole request made
// the cost grow with the square of the session length.
func TestConvertGeminiRequestToOpenAI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiSession, "deepseek-chat", ConvertGeminiRequestToOpenAI)
}

func BenchmarkConvertGeminiRequestToOpenAI_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "deepseek-chat", ConvertGeminiRequestToOpenAI)
}

func BenchmarkConvertGeminiRequestToOpenAI_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "deepseek-chat", convertGeminiRequestToOpenAIReference)
}

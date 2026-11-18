package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertGeminiRequestToGeminiCLI_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview"},
	}, ConvertGeminiRequestToGeminiCLI, convertGeminiRequestToGeminiCLIReference)
}

func TestConvertGeminiRequestToGeminiCLI_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiSession, "gemini-2.5-pro", nil, ConvertGeminiRequestToGeminiCLI, convertGeminiRequestToGeminiCLIReference)
}

// Regrouping contents through a wrapper document, and fixing every role,
// declaration and part on the whole request, made the cost grow with the
// square of the session length.
func TestConvertGeminiRequestToGeminiCLI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiSession, "gemini-2.5-pro", ConvertGeminiRequestToGeminiCLI)
}

func BenchmarkConvertGeminiRequestToGeminiCLI_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gemini-2.5-pro", ConvertGeminiRequestToGeminiCLI)
}

func BenchmarkConvertGeminiRequestToGeminiCLI_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gemini-2.5-pro", convertGeminiRequestToGeminiCLIReference)
}

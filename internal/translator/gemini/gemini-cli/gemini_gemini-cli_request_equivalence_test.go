package geminiCLI

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

func TestConvertGeminiCLIRequestToGemini_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiCLIRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview"},
	}, ConvertGeminiCLIRequestToGemini, convertGeminiCLIRequestToGeminiReference)
}

func TestConvertGeminiCLIRequestToGemini_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiCLISession, "gemini-2.5-pro", nil, ConvertGeminiCLIRequestToGemini, convertGeminiCLIRequestToGeminiReference)
}

// Renaming every declaration and marking every part on the whole request made
// the cost grow with the square of the session length.
func TestConvertGeminiCLIRequestToGemini_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiCLISession, "gemini-2.5-pro", ConvertGeminiCLIRequestToGemini)
}

func BenchmarkConvertGeminiCLIRequestToGemini_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiCLISession, "gemini-2.5-pro", ConvertGeminiCLIRequestToGemini)
}

func BenchmarkConvertGeminiCLIRequestToGemini_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiCLISession, "gemini-2.5-pro", convertGeminiCLIRequestToGeminiReference)
}

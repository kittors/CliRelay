package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertGeminiRequestToGemini_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview"},
	}, ConvertGeminiRequestToGemini, convertGeminiRequestToGeminiReference)
}

func TestConvertGeminiRequestToGemini_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiSession, "gemini-2.5-pro", nil, ConvertGeminiRequestToGemini, convertGeminiRequestToGeminiReference)
}

// Renaming every declaration and marking every part on the whole request made
// the cost grow with the square of the session length.
func TestConvertGeminiRequestToGemini_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiSession, "gemini-2.5-pro", ConvertGeminiRequestToGemini)
}

func BenchmarkConvertGeminiRequestToGemini_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gemini-2.5-pro", ConvertGeminiRequestToGemini)
}

func BenchmarkConvertGeminiRequestToGemini_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gemini-2.5-pro", convertGeminiRequestToGeminiReference)
}

package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

func TestConvertClaudeRequestToGemini_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ClaudeRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview", "gemini-2.0-flash"},
	}, ConvertClaudeRequestToGemini, convertClaudeRequestToGeminiReference)
}

func TestConvertClaudeRequestToGemini_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.ClaudeCodeSession, "gemini-2.5-pro", nil, ConvertClaudeRequestToGemini, convertClaudeRequestToGeminiReference)
}

// Appending every content, and then every tool declaration, to the whole
// request made the cost grow with the square of the session length.
func TestConvertClaudeRequestToGemini_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.ClaudeCodeSession, "gemini-2.5-pro", ConvertClaudeRequestToGemini)
}

func BenchmarkConvertClaudeRequestToGemini_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "gemini-2.5-pro", ConvertClaudeRequestToGemini)
}

func BenchmarkConvertClaudeRequestToGemini_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "gemini-2.5-pro", convertClaudeRequestToGeminiReference)
}

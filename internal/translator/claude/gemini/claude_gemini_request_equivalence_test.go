package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

// Function calls get a random toolu_ id: Gemini has none to pair them by.
var maskRandomIDs = translatortest.MaskRandomIDs("toolu_")

func TestConvertGeminiRequestToClaude_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiRequest,
		Models:   []string{"claude-sonnet-4-5", "claude-opus-4-1"},
		Mask:     maskRandomIDs,
	}, ConvertGeminiRequestToClaude, convertGeminiRequestToClaudeReference)
}

func TestConvertGeminiRequestToClaude_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiSession, "claude-sonnet-4-5", maskRandomIDs, ConvertGeminiRequestToClaude, convertGeminiRequestToClaudeReference)
}

// Appending every message, and lower-casing every schema type, on the whole
// request made the cost grow with the square of the session length.
func TestConvertGeminiRequestToClaude_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiSession, "claude-sonnet-4-5", ConvertGeminiRequestToClaude)
}

func BenchmarkConvertGeminiRequestToClaude_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "claude-sonnet-4-5", ConvertGeminiRequestToClaude)
}

func BenchmarkConvertGeminiRequestToClaude_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "claude-sonnet-4-5", convertGeminiRequestToClaudeReference)
}

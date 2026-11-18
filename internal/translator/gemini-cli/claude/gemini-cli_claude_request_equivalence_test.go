package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertClaudeRequestToCLI_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ClaudeRequest,
		Models:   []string{"gemini-2.5-pro", "gemini-3-pro-preview", "gemini-2.0-flash"},
	}, ConvertClaudeRequestToCLI, convertClaudeRequestToCLIReference)
}

func TestConvertClaudeRequestToCLI_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.ClaudeCodeSession, "gemini-2.5-pro", nil, ConvertClaudeRequestToCLI, convertClaudeRequestToCLIReference)
}

// Appending every content, and then every tool declaration, to the whole
// request made the cost grow with the square of the session length.
func TestConvertClaudeRequestToCLI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.ClaudeCodeSession, "gemini-2.5-pro", ConvertClaudeRequestToCLI)
}

func BenchmarkConvertClaudeRequestToCLI_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "gemini-2.5-pro", ConvertClaudeRequestToCLI)
}

func BenchmarkConvertClaudeRequestToCLI_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "gemini-2.5-pro", convertClaudeRequestToCLIReference)
}

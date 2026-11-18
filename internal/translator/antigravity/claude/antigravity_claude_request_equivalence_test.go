package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertClaudeRequestToAntigravity_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ClaudeRequest,
		Models:   []string{"claude-sonnet-4-5-thinking", "gemini-3-pro-preview", "claude-opus-4-5"},
	}, ConvertClaudeRequestToAntigravity, convertClaudeRequestToAntigravityReference)
}

func TestConvertClaudeRequestToAntigravity_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.ClaudeCodeSession, "claude-sonnet-4-5-thinking", nil, ConvertClaudeRequestToAntigravity, convertClaudeRequestToAntigravityReference)
}

// Appending every content to the contents built so far made the cost grow
// with the square of the session length.
func TestConvertClaudeRequestToAntigravity_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.ClaudeCodeSession, "claude-sonnet-4-5-thinking", ConvertClaudeRequestToAntigravity)
}

func BenchmarkConvertClaudeRequestToAntigravity_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "claude-sonnet-4-5-thinking", ConvertClaudeRequestToAntigravity)
}

func BenchmarkConvertClaudeRequestToAntigravity_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "claude-sonnet-4-5-thinking", convertClaudeRequestToAntigravityReference)
}

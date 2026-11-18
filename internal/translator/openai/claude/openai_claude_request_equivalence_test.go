package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

func TestConvertClaudeRequestToOpenAI_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.ClaudeRequest,
		Models:   []string{"deepseek-chat", "kimi-k2", "glm-4.6"},
	}, ConvertClaudeRequestToOpenAI, convertClaudeRequestToOpenAIReference)
}

func TestConvertClaudeRequestToOpenAI_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.ClaudeCodeSession, "deepseek-chat", nil, ConvertClaudeRequestToOpenAI, convertClaudeRequestToOpenAIReference)
}

// Appending every message to the messages built so far made the cost grow
// with the square of the session length.
func TestConvertClaudeRequestToOpenAI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.ClaudeCodeSession, "deepseek-chat", ConvertClaudeRequestToOpenAI)
}

func BenchmarkConvertClaudeRequestToOpenAI_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "deepseek-chat", ConvertClaudeRequestToOpenAI)
}

func BenchmarkConvertClaudeRequestToOpenAI_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.ClaudeCodeSession, "deepseek-chat", convertClaudeRequestToOpenAIReference)
}

package gemini

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/testutil/translatortest"
)

// Function calls get a random call_ id: Gemini has none to pair them by.
var maskRandomIDs = translatortest.MaskRandomIDs("call_")

func TestConvertGeminiRequestToCodex_MatchesReference(t *testing.T) {
	translatortest.MatchReference(t, translatortest.Equivalence{
		Seed:     20261116,
		Requests: 3000,
		Generate: translatortest.GeminiRequest,
		Models:   []string{"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex"},
		Mask:     maskRandomIDs,
	}, ConvertGeminiRequestToCodex, convertGeminiRequestToCodexReference)
}

func TestConvertGeminiRequestToCodex_MatchesReferenceOnCodexSessions(t *testing.T) {
	translatortest.MatchReferenceOnSessions(t, translatortest.GeminiSession, "gpt-5.4", maskRandomIDs, ConvertGeminiRequestToCodex, convertGeminiRequestToCodexReference)
}

// Appending every input item, and then every tool and every lower-cased
// schema type, to the whole request made the cost grow with the square of the
// session length.
func TestConvertGeminiRequestToCodex_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.GeminiSession, "gpt-5.4", ConvertGeminiRequestToCodex)
}

func BenchmarkConvertGeminiRequestToCodex_CodexSession(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gpt-5.4", ConvertGeminiRequestToCodex)
}

func BenchmarkConvertGeminiRequestToCodex_CodexSessionReference(b *testing.B) {
	translatortest.Benchmark(b, translatortest.GeminiSession, "gpt-5.4", convertGeminiRequestToCodexReference)
}

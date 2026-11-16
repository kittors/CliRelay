package responses

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

// Codex sessions routed to Gemini CLI go through the Responses -> Gemini and
// the Gemini -> Gemini CLI translators, and both used to edit the whole
// request once per element.
func TestConvertOpenAIResponsesRequestToGeminiCLI_CostGrowsLinearlyWithHistory(t *testing.T) {
	translatortest.AssertGrowsLinearly(t, translatortest.CodexResponsesSession, "gemini-2.5-pro", ConvertOpenAIResponsesRequestToGeminiCLI)
}

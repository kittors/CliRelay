package responses

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/translatortest"
)

// Codex sessions routed to Antigravity go through the Responses -> Gemini and
// the Gemini -> Antigravity translators, and both used to edit the whole
// request once per element.
func TestConvertOpenAIResponsesRequestToAntigravity_CostGrowsLinearlyWithHistory(t *testing.T) {
	for _, model := range []string{"gemini-3-pro-preview", "claude-sonnet-4-5-thinking"} {
		translatortest.AssertGrowsLinearly(t, translatortest.CodexResponsesSession, model, ConvertOpenAIResponsesRequestToAntigravity)
	}
}

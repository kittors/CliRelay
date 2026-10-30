package common

import "testing"

func TestClaudeModelTakesOutputFormat(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		// The supportedModels list, as Anthropic names them.
		{"claude-sonnet-4-5", true},
		{"claude-sonnet-4-5-20250929", true},
		{"claude-opus-4-5-20251101", true},
		{"claude-haiku-4-5-20251001", true},
		{"claude-opus-4-6", true},
		{"claude-sonnet-4-6", true},
		{"claude-opus-4-7", true},
		{"claude-opus-4-8", true},
		{"claude-opus-5", true},
		{"claude-sonnet-5", true},
		{"claude-haiku-5-5", true},
		{"claude-opus-5-5", true},
		{"claude-sonnet-5-5", true},
		{"claude-fable-5", true},
		{"claude-fable-5-1", true},
		{"claude-mythos-5", true},
		{"claude-mythos-5-1", true},
		{"claude-mythos-preview", true},
		// Before 4.5. A date is not a minor version: claude-sonnet-4-20250514 is 4.0.
		{"claude-sonnet-4-20250514", false},
		{"claude-opus-4-20250514", false},
		{"claude-opus-4-1-20250805", false},
		{"claude-opus-4-1", false},
		{"claude-opus-4-0", false},
		{"claude-opus-4", false},
		{"claude-opus-4-4", false},
		{"claude-3-7-sonnet-20250219", false},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-3-5-haiku-latest", false},
		{"claude-3-opus-20240229", false},
		{"claude-4-sonnet", false},
		{"claude-2.1", false},
		{"claude-instant-1.2", false},
		// Suffixes after the version.
		{"claude-sonnet-4-5-thinking", true},
		{"claude-opus-4-5-20251101-thinking", true},
		{"claude-sonnet-4-thinking", false},
		{"claude-sonnet-4-5(high)", true},
		// Bedrock, Vertex and aggregator spellings.
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"us.anthropic.claude-opus-4-5-20251101-v1:0", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"us.anthropic.claude-opus-4-1-20250805-v1:0", false},
		{"anthropic.claude-3-5-sonnet-20240620-v1:0", false},
		{"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"claude-sonnet-4-5@20250929", true},
		{"claude-sonnet-4@20250514", false},
		{"claude-3-5-sonnet-v2@20241022", false},
		{"publishers/anthropic/models/claude-opus-4-5", true},
		{"anthropic/claude-sonnet-4.5", true},
		{"anthropic/claude-sonnet-4", false},
		{"anthropic/claude-sonnet-4.5:beta", true},
		{"anthropic/claude-3.7-sonnet:thinking", false},
		{"claude-sonnet-4.5", true},
		{"claude-4.5-sonnet", true},
		// Case.
		{"Claude-Sonnet-4-5", true},
		{"CLAUDE-OPUS-4-1", false},
		// Not Claude, or no version to read.
		{"kimi-k2", false},
		{"kimi-k2-thinking", false},
		{"glm-4.6", false},
		{"minimax-m2", false},
		{"deepseek-chat", false},
		{"sonnet-4-5", false},
		{"gpt-5", false},
		{"claude", false},
		{"claude-sonnet", false},
		{"claude-sonnet-latest", false},
		{"", false},
	} {
		if got := claudeModelTakesOutputFormat(tc.model); got != tc.want {
			t.Errorf("claudeModelTakesOutputFormat(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestGeminiStructuredOutputSupport(t *testing.T) {
	names := map[geminiOutputSupport]string{
		geminiAskInSystem:   "ask in system",
		geminiJSONMode:      "JSON mode",
		geminiJSONSchema:    "JSON schema",
		geminiAskInUserTurn: "ask in user turn",
		geminiNoTextOutput:  "no text output",
	}
	for _, tc := range []struct {
		model string
		want  geminiOutputSupport
	}{
		{"gemini-2.5-pro", geminiJSONSchema},
		{"gemini-2.5-flash", geminiJSONSchema},
		{"gemini-2.5-flash-lite", geminiJSONSchema},
		{"gemini-2.5-pro-preview-06-05", geminiJSONSchema},
		{"gemini-3-pro-preview", geminiJSONSchema},
		{"gemini-3-pro-high", geminiJSONSchema},
		{"gemini-3-flash-agent", geminiJSONSchema},
		{"gemini-3.1-pro-low", geminiJSONSchema},
		{"gemini-flash-latest", geminiJSONSchema},
		{"gemini-flash-lite-latest", geminiJSONSchema},
		{"gemini-pro-latest", geminiJSONSchema},
		{"models/gemini-2.5-pro", geminiJSONSchema},
		{"publishers/google/models/gemini-2.5-flash@001", geminiJSONSchema},
		{"Gemini-2.5-Pro", geminiJSONSchema},
		{"gemini-2.0-flash", geminiJSONMode},
		{"gemini-2.0-flash-lite", geminiJSONMode},
		{"gemini-2.4-flash", geminiJSONMode},
		{"gemini-1.5-pro", geminiJSONMode},
		{"gemini-1.5-flash-latest", geminiJSONMode},
		{"gemini-exp-1206", geminiJSONMode},
		{"gemini-pro", geminiJSONMode},
		{"claude-sonnet-4-5-thinking", geminiAskInSystem},
		{"claude-opus-4-6-thinking", geminiAskInSystem},
		{"gpt-oss-120b-medium", geminiAskInSystem},
		{"tab_flash_lite_preview", geminiAskInSystem},
		{"", geminiAskInSystem},
		{"gemma-3-27b-it", geminiAskInUserTurn},
		{"models/gemma-3n-e4b-it", geminiAskInUserTurn},
		{"gemini-2.5-flash-image", geminiNoTextOutput},
		{"gemini-3-pro-image", geminiNoTextOutput},
		{"gemini-3.1-flash-image", geminiNoTextOutput},
		{"gemini-2.0-flash-preview-image-generation", geminiNoTextOutput},
		{"gemini-2.5-flash-preview-tts", geminiNoTextOutput},
		{"gemini-2.5-flash-native-audio-preview-09-2025", geminiNoTextOutput},
		{"gemini-live-2.5-flash-preview", geminiNoTextOutput},
		{"gemini-embedding-001", geminiNoTextOutput},
	} {
		if got := geminiStructuredOutputSupport(tc.model); got != tc.want {
			t.Errorf("geminiStructuredOutputSupport(%q) = %s, want %s", tc.model, names[got], names[tc.want])
		}
	}
}

func TestModelVersion(t *testing.T) {
	for _, tc := range []struct {
		tokens             []string
		major, minor, used int
		ok                 bool
	}{
		{[]string{"4", "5", "20250929"}, 4, 5, 2, true},
		{[]string{"4", "20250514"}, 4, 0, 1, true},
		{[]string{"4.5"}, 4, 5, 1, true},
		{[]string{"10", "1"}, 10, 1, 2, true},
		{[]string{"sonnet", "4"}, 0, 0, 0, false},
		{[]string{"4.x"}, 0, 0, 0, false},
		{[]string{"+4"}, 0, 0, 0, false},
		{[]string{"123"}, 0, 0, 0, false},
		{nil, 0, 0, 0, false},
	} {
		major, minor, used, ok := modelVersion(tc.tokens)
		if major != tc.major || minor != tc.minor || used != tc.used || ok != tc.ok {
			t.Errorf("modelVersion(%q) = %d, %d, %d, %v; want %d, %d, %d, %v", tc.tokens, major, minor, used, ok, tc.major, tc.minor, tc.used, tc.ok)
		}
	}
}

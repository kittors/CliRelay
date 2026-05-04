package thinking

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractOpenAIConfigReadsResponsesReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"glm-5.1","input":"hi","reasoning":{"effort":"medium"}}`)

	got := extractOpenAIConfig(body)

	if got.Mode != ModeLevel || got.Level != LevelMedium {
		t.Fatalf("extractOpenAIConfig() = mode %q level %q, want level medium", got.Mode, got.Level)
	}
}

func TestStripThinkingConfigOpenAIRemovesResponsesReasoning(t *testing.T) {
	body := []byte(`{"model":"glm-5.1","input":"hi","reasoning":{"effort":"medium"},"reasoning_effort":"high"}`)

	got := StripThinkingConfig(body, "openai")

	if gjson.GetBytes(got, "reasoning").Exists() {
		t.Fatalf("reasoning was not stripped: %s", string(got))
	}
	if gjson.GetBytes(got, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort was not stripped: %s", string(got))
	}
}

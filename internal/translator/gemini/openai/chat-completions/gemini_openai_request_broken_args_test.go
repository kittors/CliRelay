package chat_completions

import (
	"strings"
	"testing"
)

// Tool call arguments that are not valid JSON are copied into args as they
// are, so the request is invalid either way. Appending each content to the
// whole request re-parsed that broken document and pushed every later content
// out of the contents array; built once, they stay in place and in order.
func TestConvertOpenAIRequestToGemini_BrokenArgumentsKeepLaterContentsInPlace(t *testing.T) {
	req := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{oops"}}]},{"role":"tool","tool_call_id":"c1","content":"done"},{"role":"user","content":"after the broken call"}]}`)
	got := string(ConvertOpenAIRequestToGemini("gemini-2.5-pro", req, true))
	later, model := strings.Index(got, "after the broken call"), strings.Index(got, `"model":`)
	if later < 0 || model < 0 || later > model || strings.Count(got, "after the broken call") != 1 {
		t.Fatalf("the later user content should stay in contents, ahead of the model field: %s", got)
	}
}

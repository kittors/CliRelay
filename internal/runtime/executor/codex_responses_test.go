package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeCodexResponsesRequestStripsUnsupportedTokenLimitFields(t *testing.T) {
	input := []byte(`{"model":"gpt-5.4","max_output_tokens":1024,"max_completion_tokens":2048,"max_tokens":4096,"stream":true}`)
	got := sanitizeCodexResponsesRequest(input)

	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("%s should be stripped for codex upstream; payload=%s", field, got)
		}
	}
	if gotModel := gjson.GetBytes(got, "model").String(); gotModel != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4; payload=%s", gotModel, got)
	}
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Fatalf("stream should be preserved; payload=%s", got)
	}
}

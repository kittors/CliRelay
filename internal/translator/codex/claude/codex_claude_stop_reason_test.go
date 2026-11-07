package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexClaudeStopReason(t *testing.T) {
	for _, tc := range []struct {
		name        string
		response    string
		hasToolCall bool
		want        string
	}{
		{"completed", `{"status":"completed"}`, false, "end_turn"},
		{"completed with tool call", `{"status":"completed"}`, true, "tool_use"},
		{"cut short in a tool call", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, true, "max_tokens"},
		{"filtered", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}`, false, "refusal"},
		{"unknown incomplete reason", `{"status":"incomplete","incomplete_details":{"reason":"something_new"}}`, false, "end_turn"},
		// A stop_reason field some Responses-compatible upstreams add.
		{"anthropic stop_sequence", `{"status":"completed","stop_reason":"stop_sequence"}`, false, "stop_sequence"},
		{"anthropic max_tokens", `{"status":"completed","stop_reason":"max_tokens"}`, false, "max_tokens"},
		{"openai stop is not an anthropic name", `{"status":"completed","stop_reason":"stop"}`, false, "end_turn"},
		{"openai length", `{"status":"completed","stop_reason":"length"}`, false, "max_tokens"},
		{"tool_use without a tool call", `{"status":"completed","stop_reason":"tool_use"}`, false, "end_turn"},
	} {
		if got := codexClaudeStopReason(gjson.Parse(tc.response), tc.hasToolCall); got != tc.want {
			t.Errorf("%s: stop_reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

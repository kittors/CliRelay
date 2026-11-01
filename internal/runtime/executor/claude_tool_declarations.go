package executor

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic rejects a request whose history holds tool calls when the request
// declares no tools ("Requests which include `tool_use` or `tool_result`
// blocks must define tools"). Clients send such requests when they leave the
// tool list out of a turn over the conversation so far: a title or a summary,
// a compaction pass. Other upstreams answer them, so the same request fails
// only when it is routed to Claude.
//
// declareClaudeHistoryTools declares the tools the history's calls name, each
// with an empty schema, and sets tool_choice to none, so the model answers in
// text and cannot call a tool the client did not offer. The declarations follow
// the order the calls first appear in, so a client replaying the history gets
// the same prefix every time and prompt caching keeps working.

// claudeHistoryToolDescription describes a tool declared for the history alone.
const claudeHistoryToolDescription = "Called earlier in this conversation; not available now."

// claudeToolNameMaxLength is the longest tool name the Messages API accepts.
const claudeToolNameMaxLength = 128

// declareClaudeHistoryTools applies the declarations described above to body,
// a Claude Messages request, when it declares no tools. It returns the names
// it declared.
func declareClaudeHistoryTools(body []byte) ([]byte, []string) {
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		return body, nil
	}
	var names []string
	seen := make(map[string]struct{})
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, block gjson.Result) bool {
			if block.Get("type").String() != "tool_use" {
				return true
			}
			name := block.Get("name").String()
			if _, dup := seen[name]; dup || !isClaudeToolName(name) {
				return true
			}
			seen[name] = struct{}{}
			names = append(names, name)
			return true
		})
		return true
	})
	if len(names) == 0 {
		return body, nil
	}
	type declaration struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	declarations := make([]declaration, 0, len(names))
	for _, name := range names {
		declarations = append(declarations, declaration{Name: name, Description: claudeHistoryToolDescription, InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	raw, err := json.Marshal(declarations)
	if err != nil {
		return body, nil
	}
	updated, err := sjson.SetRawBytes(body, "tools", raw)
	if err != nil {
		return body, nil
	}
	// Any other choice either needs a tool the client offered or would let
	// the model call one of these.
	if updated, err = sjson.SetRawBytes(updated, "tool_choice", []byte(`{"type":"none"}`)); err != nil {
		return body, nil
	}
	return updated, names
}

// isClaudeToolName reports whether the Messages API accepts name for a tool. A
// call with a name it does not accept is left undeclared: the request fails as
// it did before.
func isClaudeToolName(name string) bool {
	if name == "" || len(name) > claudeToolNameMaxLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

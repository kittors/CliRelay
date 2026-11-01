package executor

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The Messages API pairs tool calls with their results strictly:
//
//   - every tool_use block of an assistant turn needs a tool_result block in
//     the user turn right after it ("`tool_use` ids were found without
//     `tool_result` blocks immediately after");
//   - every tool_result block needs its tool_use in the assistant turn right
//     before it ("unexpected `tool_use_id` found in `tool_result` blocks");
//   - in that user turn the tool_result blocks come before any other content.
//
// Histories that break this are common: a client interrupted while tools ran
// replays the call without its result, and clients that trim or merge history
// leave results whose call is gone. The upstream answers each of them with a
// 400, every time, so pairClaudeToolBlocks repairs the history before it is
// sent: a call without a result gets an error result saying none was recorded,
// and a result without a call becomes text in the same user turn.
//
// A turn is a run of consecutive messages with the same role, which the API
// combines into one. The repair only rewrites the user turns that break a rule,
// and it is deterministic, so a client replaying the same broken history gets
// the same repaired prefix every time and prompt caching keeps working.

// claudeToolPairingRepair counts what pairClaudeToolBlocks changed.
type claudeToolPairingRepair struct {
	// synthesized is the number of error results added for calls without one.
	synthesized int
	// orphaned is the number of results without a call turned into text.
	orphaned int
	// reordered is the number of user turns whose tool results were moved in
	// front of their other content.
	reordered int
}

func (r claudeToolPairingRepair) changed() bool {
	return r.synthesized > 0 || r.orphaned > 0 || r.reordered > 0
}

// claudeMissingToolResultText is the content of a result added for a call the
// history has no result for.
const claudeMissingToolResultText = "No result was recorded for this tool call."

// pairClaudeToolBlocks repairs the pairing of tool_use and tool_result blocks
// in body, a Claude Messages request; see the comment above.
func pairClaudeToolBlocks(body []byte) ([]byte, claudeToolPairingRepair) {
	var repair claudeToolPairingRepair
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, repair
	}
	turns := claudeTurnsOf(messages.Array())
	out := make([]string, 0, len(messages.Array()))
	var calls []string
	for _, turn := range turns {
		switch turn.role {
		case "assistant":
			if len(calls) > 0 {
				// Two assistant turns cannot be adjacent, so this only
				// happens after an unknown role; the results go in a user
				// turn of their own.
				out = append(out, claudeUserTurnRaw(claudeMissingToolResults(calls)))
				repair.synthesized += len(calls)
			}
			calls = claudeToolUseIDs(turn.messages)
			out = append(out, turn.raws()...)
		case "user":
			out = append(out, repairClaudeUserTurn(turn.messages, calls, &repair)...)
			calls = nil
		default:
			out = append(out, turn.raws()...)
		}
	}
	// Calls in the last turn have no user turn after them yet: the request
	// ends on the assistant's prefill, which is the client's to complete.
	if !repair.changed() {
		return body, repair
	}
	updated, err := sjson.SetRawBytes(body, "messages", []byte("["+strings.Join(out, ",")+"]"))
	if err != nil {
		return body, claudeToolPairingRepair{}
	}
	return updated, repair
}

// repairClaudeToolUse fixes what a Claude upstream would reject in the tool
// calls of a request about to go to it, and logs what it changed: the pairing
// of calls and results (pairClaudeToolBlocks), ids another upstream wrote
// (sdktranslator.EncodeClaudeRequestToolIDs) and calls in the history of a
// request that declares no tools (declareClaudeHistoryTools).
func repairClaudeToolUse(execCtx *ExecutionContext, body []byte) []byte {
	repaired, repair := pairClaudeToolBlocks(body)
	if repair.changed() {
		logWithRequestID(execCtx.Context).Debugf("claude tool pairing: added %d missing tool results, turned %d results without a call into text, moved tool results first in %d user turns", repair.synthesized, repair.orphaned, repair.reordered)
	}
	repaired = sdktranslator.EncodeClaudeRequestToolIDs(repaired)
	repaired, declared := declareClaudeHistoryTools(repaired)
	if len(declared) > 0 {
		logWithRequestID(execCtx.Context).Debugf("claude tool declarations: declared %d tools the history calls, tool_choice none", len(declared))
	}
	return repaired
}

// claudeTurn is a run of consecutive messages with one role.
type claudeTurn struct {
	role     string
	messages []gjson.Result
}

func (t claudeTurn) raws() []string {
	raws := make([]string, len(t.messages))
	for i, message := range t.messages {
		raws[i] = message.Raw
	}
	return raws
}

func claudeTurnsOf(messages []gjson.Result) []claudeTurn {
	var turns []claudeTurn
	for _, message := range messages {
		role := message.Get("role").String()
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].messages = append(turns[n-1].messages, message)
			continue
		}
		turns = append(turns, claudeTurn{role: role, messages: []gjson.Result{message}})
	}
	return turns
}

// claudeToolUseIDs lists the ids of the tool_use blocks in an assistant turn,
// in order.
func claudeToolUseIDs(messages []gjson.Result) []string {
	var ids []string
	for _, message := range messages {
		content := message.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, block := range content.Array() {
			if block.Get("type").String() != "tool_use" {
				continue
			}
			if id := block.Get("id").String(); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// repairClaudeUserTurn checks a user turn against calls, the tool_use ids of
// the assistant turn before it. A turn that keeps every rule is returned as
// it was, message by message. Otherwise the turn becomes one message, which
// the API would have made of it anyway: the results of calls first, with an
// error result for each call that has none, then the other content in its
// order, with results whose call is not in calls written as text.
func repairClaudeUserTurn(messages []gjson.Result, calls []string, repair *claudeToolPairingRepair) []string {
	expected := make(map[string]bool, len(calls))
	for _, id := range calls {
		expected[id] = true
	}
	var results, rest []string
	answered := make(map[string]bool, len(calls))
	orphaned, misplaced := 0, false
	for _, message := range messages {
		content := message.Get("content")
		if !content.IsArray() {
			if content.Type == gjson.String && content.String() != "" {
				block, _ := sjson.Set(`{"type":"text"}`, "text", content.String())
				rest = append(rest, block)
			}
			continue
		}
		for _, block := range content.Array() {
			if block.Get("type").String() != "tool_result" {
				rest = append(rest, block.Raw)
				continue
			}
			id := block.Get("tool_use_id").String()
			if !expected[id] {
				rest = append(rest, claudeOrphanToolResultBlocks(block)...)
				orphaned++
				continue
			}
			if len(rest) > 0 {
				misplaced = true
			}
			answered[id] = true
			results = append(results, block.Raw)
		}
	}
	var missing []string
	for _, id := range calls {
		if !answered[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 && orphaned == 0 && !misplaced {
		return claudeTurn{messages: messages}.raws()
	}
	repair.synthesized += len(missing)
	repair.orphaned += orphaned
	if misplaced {
		repair.reordered++
	}
	blocks := append(append(claudeMissingToolResults(missing), results...), rest...)
	// The first message keeps its other fields (cache_control on the
	// message, for one); only its content changes.
	merged, err := sjson.SetRaw(messages[0].Raw, "content", "["+strings.Join(blocks, ",")+"]")
	if err != nil {
		return []string{claudeUserTurnRaw(blocks)}
	}
	return []string{merged}
}

// claudeMissingToolResults returns an error result for each id.
func claudeMissingToolResults(ids []string) []string {
	blocks := make([]string, 0, len(ids))
	for _, id := range ids {
		block, _ := sjson.Set(`{"type":"tool_result"}`, "tool_use_id", id)
		block, _ = sjson.Set(block, "is_error", true)
		block, _ = sjson.Set(block, "content", claudeMissingToolResultText)
		blocks = append(blocks, block)
	}
	return blocks
}

// claudeOrphanToolResultBlocks writes a result whose call is not in the turn
// before it as content the user turn can carry: a text block naming the call,
// then the result's own text and image blocks.
func claudeOrphanToolResultBlocks(result gjson.Result) []string {
	header := "Result of tool call " + result.Get("tool_use_id").String()
	if result.Get("is_error").Bool() {
		header += " (error)"
	}
	header += ":"
	content := result.Get("content")
	var texts, others []string
	switch {
	case content.Type == gjson.String:
		texts = append(texts, content.String())
	case content.IsArray():
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "text":
				texts = append(texts, block.Get("text").String())
			case "image", "document":
				others = append(others, block.Raw)
			}
		}
	}
	text := header
	if len(texts) > 0 {
		text += "\n" + strings.Join(texts, "\n")
	}
	block, _ := sjson.Set(`{"type":"text"}`, "text", text)
	return append([]string{block}, others...)
}

func claudeUserTurnRaw(blocks []string) string {
	return `{"role":"user","content":[` + strings.Join(blocks, ",") + `]}`
}

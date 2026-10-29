package common

import "strings"

// Every protocol says why a reply ended, each with its own names, and each has
// reasons the others lack: Anthropic's refusal and model_context_window_exceeded,
// Chat's content_filter, Gemini's SAFETY and RECITATION. A translator that maps
// only the names it knows sends the rest on as a plain end of turn, so a client
// is never told that its reply was cut short or withheld. Writing a name the
// client's protocol does not define ("stop" or "max_tokens" where the other
// protocol expects something else) is no better: SDKs reject or misread it.
//
// Every translator reads the upstream's reason through one of the
// StopReasonFrom* functions and writes it with one of the StopReason methods,
// so every pair of protocols maps the same way.

// StopReason is why a reply ended, in terms every protocol can express.
type StopReason int

const (
	// StopEndTurn is a natural end of the turn, a stop sequence, or any reason
	// the other protocols have no closer name for.
	StopEndTurn StopReason = iota
	// StopMaxTokens is a reply cut short by the output token limit, or by the
	// context window filling up first.
	StopMaxTokens
	// StopToolUse is a reply that calls tools.
	StopToolUse
	// StopRefusal is a reply the model refused or the upstream filtered.
	StopRefusal
)

// StopReasonFromClaude reads an Anthropic stop_reason. Anthropic-shaped relays
// in front of other vendors' models pass OpenAI's names on as they came, so
// those are read too.
func StopReasonFromClaude(reason string) StopReason {
	switch reason {
	case "max_tokens", "model_context_window_exceeded", "length":
		return StopMaxTokens
	case "tool_use", "tool_calls", "function_call":
		return StopToolUse
	case "refusal", "content_filter":
		return StopRefusal
	}
	// end_turn and stop_sequence, and pause_turn: a server tool paused a long
	// turn, which the other protocols have no way to resume.
	return StopEndTurn
}

// IsClaudeStopReason reports whether reason is one of the stop_reason values
// Anthropic defines, so an Anthropic client can be given it as it is.
func IsClaudeStopReason(reason string) bool {
	switch reason {
	case "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal", "model_context_window_exceeded":
		return true
	}
	return false
}

// StopReasonFromOpenAI reads a Chat Completions finish_reason. Some
// OpenAI-compatible upstreams write max_tokens for length.
func StopReasonFromOpenAI(reason string) StopReason {
	switch reason {
	case "length", "max_tokens":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopRefusal
	}
	return StopEndTurn
}

// StopReasonFromGemini reads a Gemini finishReason. Gemini has no reason for
// tool calls: a reply that calls functions ends with STOP, and translators
// tell it apart by its functionCall parts.
func StopReasonFromGemini(reason string) StopReason {
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return StopRefusal
	}
	return StopEndTurn
}

// StopReasonFromResponses reads a Responses API response from its status and
// incomplete_details.reason. Like Gemini, it has no reason for tool calls.
func StopReasonFromResponses(status, incompleteReason string) StopReason {
	if status != "incomplete" {
		return StopEndTurn
	}
	switch incompleteReason {
	case "max_output_tokens":
		return StopMaxTokens
	case "content_filter":
		return StopRefusal
	}
	return StopEndTurn
}

// Claude returns the Anthropic stop_reason for r.
func (r StopReason) Claude() string {
	switch r {
	case StopMaxTokens:
		return "max_tokens"
	case StopToolUse:
		return "tool_use"
	case StopRefusal:
		return "refusal"
	}
	return "end_turn"
}

// OpenAI returns the Chat Completions finish_reason for r.
func (r StopReason) OpenAI() string {
	switch r {
	case StopMaxTokens:
		return "length"
	case StopToolUse:
		return "tool_calls"
	case StopRefusal:
		return "content_filter"
	}
	return "stop"
}

// Gemini returns the Gemini finishReason for r; tool calls end with STOP.
func (r StopReason) Gemini() string {
	switch r {
	case StopMaxTokens:
		return "MAX_TOKENS"
	case StopRefusal:
		return "SAFETY"
	}
	return "STOP"
}

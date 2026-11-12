package openai

import (
	"bytes"
	"io"
)

// writeResponsesSSEChunk frames translated event/data pairs and data-only chunks
// immediately, while leaving metadata-only chunks attached to the next data.
// Metadata-only chunks must not dispatch before their data arrives (see #1176).
// Codex Responses passthrough assembles full upstream frames in its executor;
// other built-in translators provide complete event/data or data-only chunks.
// Custom executors must likewise supply complete data-bearing frames; arbitrary
// metadata split across handler keepalives is not guaranteed to stay attached.
// Existing delimiters are preserved, including chunks containing several events.
func writeResponsesSSEChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	_, _ = w.Write(chunk)

	// Only an unfinished event containing a nonempty data payload needs a
	// blank-line terminator. Empty data lines can precede another data line,
	// including when Codex forwards those lines as separate chunks.
	// SSE accepts LF, CRLF and CR line endings. A blank line ends the current
	// event, so data in an earlier, already-framed event must not terminate a
	// trailing metadata-only event in the same chunk.
	hasPayload := false
	for start := 0; start < len(chunk); {
		end := start
		for end < len(chunk) && chunk[end] != '\n' && chunk[end] != '\r' {
			end++
		}
		line := chunk[start:end]
		if len(line) == 0 {
			hasPayload = false
		} else if field, value, _ := bytes.Cut(line, []byte(":")); bytes.Equal(field, []byte("data")) && len(bytes.TrimSpace(value)) > 0 {
			hasPayload = true
		}
		if end == len(chunk) {
			break
		}
		start = end + 1
		if chunk[end] == '\r' && start < len(chunk) && chunk[start] == '\n' {
			start++
		}
	}
	last := chunk[len(chunk)-1]
	if last != '\n' && last != '\r' {
		_, _ = w.Write([]byte("\n"))
	}
	if hasPayload {
		// An LF immediately after CR forms one CRLF line ending, not a blank
		// line. Supply another LF so a CR-terminated data line dispatches too.
		if last == '\r' {
			_, _ = w.Write([]byte("\n"))
		}
		_, _ = w.Write([]byte("\n"))
	}
}

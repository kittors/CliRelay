package executor

import (
	"bytes"
	"context"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxCodexSSELineBytes = 50 << 20

// Both completed and incomplete are normal upstream replies. Keep status
// optional for existing compatible providers, but never accept a bare event
// name or malformed JSON as proof that an attempt finished.
// normalizeCodexResponseDone rewrites an upstream response.done event to the
// response.completed event the Responses clients expect. Codex upstream closes
// some turns with response.done; without this the relay forwards no terminal
// event and the client reports "stream closed before response.completed".
func normalizeCodexResponseDone(payload []byte) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() || root.Get("type").String() != "response.done" || !root.Get("response").IsObject() {
		return payload, false
	}
	normalized, err := sjson.SetRawBytes(payload, "type", []byte(`"response.completed"`))
	if err != nil {
		return payload, false
	}
	// The payload's own status follows the event name so a non-streaming
	// translation does not hand the client status "done".
	if root.Get("response.status").String() == "done" {
		if updated, errStatus := sjson.SetRawBytes(normalized, "response.status", []byte(`"completed"`)); errStatus == nil {
			normalized = updated
		}
	}
	return normalized, true
}

func validCodexResponsesTerminal(payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	return root.IsObject() && root.Get("type").Type == gjson.String && codexResponseFinished(root.Get("type").String()) && root.Get("response").IsObject()
}

// Do not publish success while the stream can still report a provider error.
// A transport close after a valid terminal is cleanup, but an explicit error
// must win even if that terminal carried usage and the consumer then cancelled.
type codexResponsesTerminal struct {
	seen      bool
	usageSeen bool
	usage     coreusage.Detail
}

func (t *codexResponsesTerminal) observe(payload []byte) {
	if !validCodexResponsesTerminal(payload) {
		return
	}
	t.seen = true
	if detail, ok := parseCodexUsage(payload); ok && !t.usageSeen {
		t.usage = detail
		// Match usageReporter's first nonzero publication semantics: a duplicate
		// terminal must not erase (or replace) tokens already reported upstream.
		t.usageSeen = detail.InputTokens != 0 || detail.OutputTokens != 0 || detail.ReasoningTokens != 0 || detail.CachedTokens != 0 || detail.TotalTokens != 0
	}
}
func (t *codexResponsesTerminal) publishSuccess(ctx context.Context, reporter *usageReporter) {
	reporter.publish(ctx, t.usage)
	reporter.ensurePublished(ctx)
}
func (t *codexResponsesTerminal) publishFailure(ctx context.Context, reporter *usageReporter) {
	reporter.publishWithOutcome(ctx, t.usage, true)
}

// normalizeCodexResponseDoneLines rewrites the response.done data frames of one
// emitted SSE event in place, so the frames forwarded to the client and the
// copies inspected for the terminal event agree.
func normalizeCodexResponseDoneLines(eventLines [][]byte) {
	for i, eventLine := range eventLines {
		line := bytes.TrimSuffix(eventLine, []byte{'\r'})
		field, value := codexSSEField(line)
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		if normalized, ok := normalizeCodexResponseDone(value); ok {
			eventLines[i] = append([]byte("data: "), normalized...)
		}
	}
}

// codexResponsesSSEPayloads inspects complete JSON emitted by the stream framer
// without changing its wire lines. Ordinary single-line JSON remains immediate;
// fragmented data fields are joined at their blank delimiter or clean EOF.
func codexResponsesSSEPayloads(lines [][]byte) [][]byte {
	var payloads [][]byte
	var data [][]byte
	flush := func() {
		if len(data) > 0 {
			payloads = append(payloads, bytes.Join(data, []byte{'\n'}))
			data = nil
		}
	}
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			flush()
			continue
		}
		field, value := codexSSEField(line)
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		data = append(data, value)
		if len(data) == 1 && gjson.ValidBytes(value) {
			flush()
		}
	}
	flush()
	return payloads
}

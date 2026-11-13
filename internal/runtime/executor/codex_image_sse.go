package executor

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
)

// The scanner caps one line at 50 MiB. Also bound the accumulated prefix / terminal
// event: an upstream must not grow memory indefinitely with metadata or partial JSON.
const maxCodexImageSSEBufferedBytes = 50 << 20

// codexImageSSEFramer keeps an event's metadata with its data. Ordinary single-line
// JSON events are released without waiting for a blank line, preserving the usual
// first-payload timeout boundary. Fragmented / non-JSON data waits for a delimiter.
// So does an event needing the image fallback, keeping even trailing event/id
// fields attached to the original event rather than the synthetic one.
// Responses passthrough instead waits for the actual blank delimiter (or clean
// EOF), so late metadata and additional data fields remain in the same event.
// Leading comments flow immediately, preserving upstream heartbeats.
type codexImageSSEFramer struct {
	normalizer *codexImageStreamNormalizer
	// Responses passthrough must retain late metadata and multi-data boundaries.
	waitForDelimiter bool
	pending          [][]byte
	data             [][]byte
	synthetic        [][]byte
	buffered         int
	streaming        bool
	limit            int // zero uses maxCodexImageSSEBufferedBytes; small values aid boundary tests
}

func (f *codexImageSSEFramer) push(line []byte) ([][]byte, error) {
	if len(line) == 0 {
		if err := f.validateResponsesEvent(); err != nil {
			return nil, err
		}
		return f.finish(true), nil
	}
	if f.streaming || (len(f.pending) == 0 && line[0] == ':') {
		return [][]byte{bytes.Clone(line)}, nil
	}
	limit := f.limit
	if limit <= 0 {
		limit = maxCodexImageSSEBufferedBytes
	}
	// Count each line's slice as well as its bytes, bounding many tiny fields too.
	cost := len(line) + 24
	if cost > limit-f.buffered {
		return nil, errors.New("codex SSE event exceeds buffered size limit")
	}
	line = bytes.Clone(line)
	f.pending = append(f.pending, line)
	f.buffered += cost
	if len(f.synthetic) > 0 {
		return nil, nil
	}
	field, value := codexSSEField(line)
	if !bytes.Equal(field, []byte("data")) {
		return nil, nil
	}
	f.data = append(f.data, value)
	// The usual one-data-line event remains immediate. If its JSON is partial,
	// wait for the delimiter and parse the joined document once, avoiding quadratic
	// copying / validation for an upstream sending many tiny data fields.
	if f.waitForDelimiter || len(f.data) > 1 || !gjson.ValidBytes(value) {
		return nil, nil
	}
	f.normalize(value)
	if len(f.synthetic) > 0 {
		return nil, nil
	}
	out := f.pending
	f.pending = nil
	f.buffered = 0
	f.streaming = true
	return out, nil
}

func (f *codexImageSSEFramer) normalize(payload []byte) {
	// The normalizer / translators consume a JSON document on one line. Compact
	// only the working copy of a multi-data payload; an unchanged event still
	// leaves using its exact original lines.
	if len(f.data) > 1 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, payload); err == nil {
			payload = compact.Bytes()
		}
	}
	canonical := append([]byte("data: "), payload...)
	if f.waitForDelimiter && len(f.data) > 1 {
		// The Responses SDK validates JSON per data line. Canonicalize only
		// fragmented JSON, not empty data fields surrounding a complete value.
		for _, part := range f.data {
			if len(bytes.TrimSpace(part)) > 0 && !gjson.ValidBytes(part) {
				f.pending = replaceCodexSSEData(f.pending, canonical)
				break
			}
		}
	}
	events := normalizeCodexImageGenerationOutboundEventWithState(f.normalizer, canonical)
	if len(events) == 0 {
		events = [][]byte{canonical}
	}
	original := events[len(events)-1]
	if len(events) > 1 || !bytes.Equal(original, canonical) {
		f.pending = replaceCodexSSEData(f.pending, original)
	}
	f.data = nil
	if len(events) > 1 {
		f.synthetic = events[:len(events)-1]
	}
}

func (f *codexImageSSEFramer) finish(terminated bool) [][]byte {
	if len(f.data) > 0 {
		payload := bytes.Join(f.data, []byte{'\n'})
		if gjson.ValidBytes(payload) {
			f.normalize(payload)
		}
	}
	out := f.pending
	if len(f.synthetic) > 0 {
		out = frameCodexImageStreamEvents(f.pending, f.synthetic)
		// A synthetic event and its terminal original must be independently
		// dispatchable, including the existing clean-EOF compatibility path.
		terminated = true
	}
	if terminated {
		out = append(out, []byte{})
	}
	f.pending, f.data, f.synthetic = nil, nil, nil
	f.buffered, f.streaming = 0, false
	return out
}

// codexSSEField follows SSE field syntax: split at the first colon and remove at
// most one optional space. A colon-less field has an empty value.
func codexSSEField(line []byte) ([]byte, []byte) {
	field, value, _ := bytes.Cut(line, []byte{':'})
	return field, bytes.TrimPrefix(value, []byte{' '})
}

func replaceCodexSSEData(lines [][]byte, replacement []byte) [][]byte {
	out := make([][]byte, 0, len(lines))
	written := false
	for _, line := range lines {
		field, _ := codexSSEField(line)
		if bytes.Equal(field, []byte("data")) {
			if !written {
				out = append(out, replacement)
				written = true
			}
			continue
		}
		out = append(out, line)
	}
	return out
}

// Synthetic events precede *all* metadata of the original event. Keep original
// metadata before data on this path for line-oriented translated consumers.
// Unchanged events never use this rearrangement and retain their original lines.
func frameCodexImageStreamEvents(original, synthetic [][]byte) [][]byte {
	out := make([][]byte, 0, 3*len(synthetic)+len(original))
	for _, event := range synthetic {
		if eventType := gjson.GetBytes(sseJSONBody(event), "type").String(); eventType != "" {
			out = append(out, []byte("event: "+eventType))
		}
		out = append(out, event, []byte{})
	}
	for _, line := range original {
		field, _ := codexSSEField(line)
		if !bytes.Equal(field, []byte("data")) {
			out = append(out, line)
		}
	}
	for _, line := range original {
		field, _ := codexSSEField(line)
		if bytes.Equal(field, []byte("data")) {
			out = append(out, line)
		}
	}
	return out
}

// Validate the whole Responses data document before interpreting any individual
// line as terminal. Two separately valid JSON values are not one valid event.
func (f *codexImageSSEFramer) validateResponsesEvent() error {
	if !f.waitForDelimiter || len(f.data) == 0 {
		return nil
	}
	payload := bytes.TrimSpace(bytes.Join(f.data, []byte{'\n'}))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || gjson.ValidBytes(payload) {
		return nil
	}
	return errors.New("invalid Codex Responses SSE event JSON")
}

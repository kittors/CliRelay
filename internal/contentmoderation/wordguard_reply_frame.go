package contentmoderation

import "bytes"

// Executors hand a stream on in different shapes. A Claude passthrough sends
// every line of the upstream's event stream as a chunk of its own; translators
// send whole events, sometimes several in one chunk; the OpenAI chat and the
// Gemini translators send the bare JSON of an event and leave the "data:"
// framing to the HTTP handler; the Responses translator sends an event without
// its blank line and the handler adds it. The filter has to cut text out of
// the middle of an event and hold events back, so it first puts the chunks
// back together into events, without changing a byte of them.

// frameKind says what the filter does with a frame.
type frameKind uint8

const (
	// frameData is an event with one JSON document to look at.
	frameData frameKind = iota
	// frameMeta is nothing but comments (keep-alives): there is nothing to
	// filter and nothing to wait for, so it passes at once, ahead of anything
	// held back. That is safe because a comment says nothing about the events
	// around it.
	frameMeta
	// frameOther is anything else the filter cannot read: a [DONE] marker, an
	// event without data, one whose data is not JSON, one with several data
	// lines. It passes in order, after the held text has been released.
	frameOther
)

// replyFrame is one event as it appeared on the wire.
type replyFrame struct {
	raw  []byte
	kind frameKind
	// data is the event's JSON document: raw[dataAt : dataAt+len(data)].
	data   []byte
	dataAt int
}

// maxReplyFrame bounds what is buffered while an event is incomplete. A
// stream that sends more without ending an event is passed on as it is rather
// than held.
const maxReplyFrame = 16 << 20

// splitFrames cuts buf into complete events and the unfinished remainder. The
// events, in order, followed by the remainder are exactly buf.
//
// An event ends at a blank line. A new "event:" line after a "data:" line also
// starts a new event, for producers that leave the blank line out.
func splitFrames(buf []byte) (frames [][]byte, rest []byte) {
	start := 0          // where the event being read began
	sawContent := false // the event has a line other than a blank one
	sawData := false
	for pos := 0; pos < len(buf); {
		end := bytes.IndexByte(buf[pos:], '\n')
		if end < 0 {
			break // an unfinished line: part of the remainder
		}
		line := buf[pos : pos+end]
		next := pos + end + 1
		switch trimmed := bytes.TrimRight(line, "\r"); {
		case len(trimmed) == 0:
			if sawContent {
				frames = append(frames, buf[start:next])
				start, sawContent, sawData = next, false, false
			}
			// A blank line with no event before it belongs to the event after it.
		case sawData && bytes.HasPrefix(trimmed, []byte("event:")):
			frames = append(frames, buf[start:pos])
			start, sawData = pos, false
			sawContent = true
		default:
			sawContent = true
			if bytes.HasPrefix(trimmed, []byte("data:")) {
				sawData = true
			}
		}
		pos = next
	}
	return frames, buf[start:]
}

// classifyFrame reads an event, found in raw, for its JSON document.
func classifyFrame(raw []byte) replyFrame {
	frame := replyFrame{raw: raw, kind: frameOther}
	datas, comments, others := 0, 0, 0
	for pos := 0; pos < len(raw); {
		end := bytes.IndexByte(raw[pos:], '\n')
		lineEnd, next := len(raw), len(raw)
		if end >= 0 {
			lineEnd, next = pos+end, pos+end+1
		}
		line := bytes.TrimRight(raw[pos:lineEnd], "\r")
		switch {
		case len(line) == 0:
		case line[0] == ':':
			comments++
		case bytes.HasPrefix(line, []byte("data:")):
			datas++
			from := pos + len("data:")
			if from < pos+len(line) && raw[from] == ' ' {
				from++
			}
			frame.dataAt, frame.data = from, raw[from:pos+len(line)]
		default:
			others++
		}
		pos = next
	}
	switch {
	case datas == 0 && others == 0 && comments > 0:
		frame.kind = frameMeta
	case datas == 1:
		frame.kind = dataFrameKind(frame.data)
	}
	if frame.kind != frameData {
		frame.data = nil
	}
	return frame
}

// dataFrameKind says whether a data field holds a JSON document.
func dataFrameKind(data []byte) frameKind {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && (data[0] == '{' || data[0] == '[') {
		return frameData
	}
	return frameOther
}

// bareJSONFrame recognises a chunk that is the JSON of one event and nothing
// else, which is how the OpenAI chat and Gemini translators hand events on.
func bareJSONFrame(chunk []byte) (replyFrame, bool) {
	// A legacy Gemini array stream puts commas between the documents.
	from := 0
	for from < len(chunk) && (chunk[from] <= ' ' || chunk[from] == ',') {
		from++
	}
	if from == len(chunk) || (chunk[from] != '{' && chunk[from] != '[') {
		return replyFrame{}, false
	}
	to := len(chunk)
	for to > from && chunk[to-1] <= ' ' {
		to--
	}
	return replyFrame{raw: chunk, kind: frameData, data: chunk[from:to], dataAt: from}, true
}

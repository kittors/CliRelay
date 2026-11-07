package contentmoderation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Events as the protocols' translators and executors produce them.

func claudeEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func claudeText(index int, text string) string {
	return claudeEvent("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%s}}`, index, quoteJSON(text)))
}

const (
	claudeStart = `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n"
	claudeBlock = `event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	claudeStop  = `event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n"
	claudePing  = `event: ping` + "\n" + `data: {"type": "ping"}` + "\n\n"
	claudeEnd   = `event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" +
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"
)

func chatChunk(text string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, quoteJSON(text))
}

func responsesDelta(text string) string {
	return fmt.Sprintf(`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"delta":%s}`, quoteJSON(text))
}

func geminiChunk(text string) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"text":%s}]},"index":0}],"usageMetadata":{"totalTokenCount":3}}`, quoteJSON(text))
}

// readerOf is what a client reads out of the chunks of a stream: the answer
// text its protocol carries in incremental events.
func readerOf(format sdktranslator.Format, chunks [][]byte) string {
	var text strings.Builder
	for _, doc := range docsOf(chunks) {
		switch format {
		case sdktranslator.FormatClaude:
			if gjson.Get(doc, "type").String() == "content_block_delta" && gjson.Get(doc, "delta.type").String() == "text_delta" {
				text.WriteString(gjson.Get(doc, "delta.text").String())
			}
		case sdktranslator.FormatOpenAI:
			text.WriteString(gjson.Get(doc, "choices.0.delta.content").String())
		case sdktranslator.FormatOpenAIResponse:
			if gjson.Get(doc, "type").String() == "response.output_text.delta" {
				text.WriteString(gjson.Get(doc, "delta").String())
			}
		case sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI:
			root := gjson.Parse(doc)
			if wrapped := root.Get("response"); wrapped.IsObject() {
				root = wrapped
			}
			text.WriteString(root.Get("candidates.0.content.parts.0.text").String())
		}
	}
	return text.String()
}

// docsOf lists the JSON documents the chunks carry, as a client would read
// them off the wire: the data lines of event streams, or the bare JSON chunks
// that the HTTP handler frames.
func docsOf(chunks [][]byte) []string {
	var docs []string
	for _, chunk := range chunks {
		found := false
		for _, line := range strings.Split(string(chunk), "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "data:") {
				docs = append(docs, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				found = true
			}
		}
		if text := strings.TrimSpace(string(chunk)); !found && strings.HasPrefix(text, "{") {
			docs = append(docs, text)
		}
	}
	return docs
}

// runStream pushes the chunks through a stream filter built for the contexts.
func runStream(t *testing.T, contexts []wordContext, format sdktranslator.Format, chunks ...string) ([][]byte, *WordGuardSnapshot) {
	t.Helper()
	snapshot := newWordGuardSnapshot(WordVerdict{Action: WordVerdictAllow}, nil, "")
	engine := newReplyEngine(contexts, snapshot)
	if engine == nil {
		t.Fatal("no reply engine")
	}
	stream := newWordReplyStream(engine, format)
	var out [][]byte
	for _, chunk := range chunks {
		out = append(out, stream.Push([]byte(chunk))...)
	}
	out = append(out, stream.Flush()...)
	for i, chunk := range out {
		// Everything a client reads must stay parseable.
		for _, doc := range docsOf([][]byte{chunk}) {
			if !gjson.Valid(doc) {
				t.Fatalf("chunk %d carries invalid JSON: %q", i, doc)
			}
		}
	}
	return out, snapshot
}

func joined(chunks [][]byte) string { return string(bytes.Join(chunks, nil)) }

func TestReplyStreamClaudeKeywordSplitAcrossEvents(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	input := []string{
		claudeStart, claudeBlock, claudePing,
		claudeText(0, "hello bad"), claudeText(0, "word again, bad"), claudeText(0, "ly"),
		claudeStop, claudeEnd,
	}
	out, snapshot := runStream(t, contexts, sdktranslator.FormatClaude, input...)
	if got := readerOf(sdktranslator.FormatClaude, out); got != "hello *** again, badly" {
		t.Fatalf("the client reads %q", got)
	}
	// One event out for each event in, in the same order; events that carry no
	// text are untouched.
	if len(out) != len(input)+1 { // claudeEnd holds two events
		t.Fatalf("%d chunks out for %d in", len(out), len(input))
	}
	for i, want := range []string{claudeStart, claudeBlock, claudePing} {
		if string(out[i]) != want {
			t.Fatalf("event %d changed:\n%q\n%q", i, out[i], want)
		}
	}
	if string(out[len(out)-3]) != claudeStop || joined(out[len(out)-2:]) != claudeEnd {
		t.Fatalf("closing events changed: %q", joined(out[len(out)-3:]))
	}
	// The text that waited moved forward, so the first delta lost its tail.
	if first := gjson.Get(docsOf(out[3:4])[0], "delta.text").String(); first != "hello " {
		t.Fatalf("first delta = %q", first)
	}
	if view := snapshot.view.Reply; view == nil || view.Hits != 1 || view.Replaced != 1 {
		t.Fatalf("counters = %+v", view)
	}
}

func TestReplyStreamKeepsEveryByteWhenNothingMatches(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("forbidden"))
	input := []string{claudeStart, claudeBlock, claudeText(0, "plain, "), claudeText(0, "ordinary text \"quoted\"\n"), claudeStop, claudeEnd}
	out, _ := runStream(t, contexts, sdktranslator.FormatClaude, input...)
	if joined(out) != strings.Join(input, "") {
		t.Fatalf("bytes changed:\n%q\n%q", joined(out), strings.Join(input, ""))
	}
}

// However the executor cuts the stream into chunks, the client gets the same
// bytes.
func TestReplyStreamChunkShapesAreEquivalent(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	events := []string{
		claudeStart, claudeBlock, claudePing,
		claudeText(0, "x bad"), claudeText(0, "wo"), claudeText(0, "rd y"),
		claudeStop, claudeEnd,
	}
	whole := strings.Join(events, "")
	reference, _ := runStream(t, contexts, sdktranslator.FormatClaude, events...)
	want := joined(reference)
	if readerOf(sdktranslator.FormatClaude, reference) != "x *** y" {
		t.Fatalf("reference reads %q", readerOf(sdktranslator.FormatClaude, reference))
	}

	var lines []string // a Claude passthrough sends every line as a chunk
	for _, line := range strings.SplitAfter(whole, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	shapes := map[string][]string{
		"one chunk":      {whole},
		"line by line":   lines,
		"event by event": events,
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for trial := 0; trial < 50; trial++ {
		var grouped []string
		for i := 0; i < len(lines); {
			n := 1 + rng.IntN(5)
			grouped = append(grouped, strings.Join(lines[i:min(i+n, len(lines))], ""))
			i += n
		}
		shapes[fmt.Sprintf("random grouping %d", trial)] = grouped
	}
	for name, chunks := range shapes {
		out, _ := runStream(t, contexts, sdktranslator.FormatClaude, chunks...)
		if joined(out) != want {
			t.Fatalf("%s: client bytes differ\n%q\n%q", name, joined(out), want)
		}
	}
}

func TestReplyStreamCommentsPassAtOnceAndDoNotReleaseHeldText(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	engine := newReplyEngine(contexts, nil)
	stream := newWordReplyStream(engine, sdktranslator.FormatClaude)

	if out := stream.Push([]byte(claudeText(0, "so bad"))); len(out) != 0 {
		t.Fatalf("an event ending in the start of a match went out: %q", joined(out))
	}
	if _, held := stream.HeldSince(); !held {
		t.Fatal("nothing is held")
	}
	out := stream.Push([]byte(": keep-alive\n\n"))
	if len(out) != 1 || string(out[0]) != ": keep-alive\n\n" {
		t.Fatalf("a comment must pass at once, got %q", joined(out))
	}
	if _, held := stream.HeldSince(); !held {
		t.Fatal("a comment released the held text")
	}
	out = stream.Push([]byte(claudePing))
	if len(out) != 1 || string(out[0]) != claudePing {
		t.Fatalf("a ping must pass at once, got %q", joined(out))
	}
	// The word still completes across the comment and the ping.
	out = stream.Push([]byte(claudeText(0, "word!")))
	if got := readerOf(sdktranslator.FormatClaude, out); got != "so ***!" {
		t.Fatalf("the client reads %q", got)
	}
	if _, held := stream.HeldSince(); held {
		t.Fatal("still holding after the word was decided")
	}
}

func TestReplyStreamReleasesHeldTextBeforeTheNextEvent(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	cases := map[string][]string{
		"block ends":   {claudeText(0, "so bad"), claudeStop, claudeEnd},
		"stream ends":  {claudeText(0, "so bad")},
		"other block":  {claudeText(0, "so bad"), claudeText(1, "word")},
		"other events": {claudeText(0, "so bad"), claudeEnd},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out, _ := runStream(t, contexts, sdktranslator.FormatClaude, input...)
			text := readerOf(sdktranslator.FormatClaude, out)
			if !strings.HasPrefix(text, "so bad") {
				t.Fatalf("the held text was lost or reordered: %q", text)
			}
			// No event is lost or added, and the held one comes out first.
			if got, want := strings.Count(joined(out), "event: "), strings.Count(strings.Join(input, ""), "event: "); got != want {
				t.Fatalf("%d events out for %d in", got, want)
			}
			if name == "other block" && readerOf(sdktranslator.FormatClaude, out) != "so badword" {
				t.Fatalf("a word split across two blocks is two words: %q", text)
			}
		})
	}
}

func TestReplyStreamFlushReleasesWhatIsHeld(t *testing.T) {
	engine := newReplyEngine(replyContexts(t, WordActionReplace, kw("bad"), kw("badword")), nil)
	stream := newWordReplyStream(engine, sdktranslator.FormatClaude)
	if out := stream.Push([]byte(claudeText(0, "so bad"))); len(out) != 0 {
		t.Fatalf("got %q", joined(out))
	}
	out := stream.Flush()
	if got := readerOf(sdktranslator.FormatClaude, out); got != "so ***" {
		t.Fatalf("the end of the held text is final on a flush: %q", got)
	}
	if _, held := stream.HeldSince(); held {
		t.Fatal("still holding after a flush")
	}
	if len(stream.Flush()) != 0 {
		t.Fatal("a second flush produced output")
	}
}

func TestReplyStreamLeavesReasoningAndToolCallsAlone(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("secret"))
	thinking := claudeEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"the secret plan"}}`)
	signature := claudeEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"secret-sig"}}`)
	tool := claudeEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"secret\"}"}}`)
	input := []string{claudeStart, thinking, signature, tool, claudeText(2, "a secret answer"), claudeEnd}
	out, _ := runStream(t, contexts, sdktranslator.FormatClaude, input...)
	want := strings.Join([]string{claudeStart, thinking, signature, tool, claudeText(2, "a *** answer"), claudeEnd}, "")
	if joined(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", joined(out), want)
	}
}

func TestReplyStreamChatChunks(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	input := []string{chatChunk("hello bad"), chatChunk("word and bad"), chatChunk("ly"),
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":4}}`}
	out, _ := runStream(t, contexts, sdktranslator.FormatOpenAI, input...)
	if got := readerOf(sdktranslator.FormatOpenAI, out); got != "hello *** and badly" {
		t.Fatalf("the client reads %q", got)
	}
	if len(out) != len(input) {
		t.Fatalf("%d chunks out for %d in", len(out), len(input))
	}
	for _, chunk := range out {
		if !bytes.HasPrefix(chunk, []byte(`{"id":"chatcmpl-1"`)) {
			t.Fatalf("a chat chunk must stay a bare JSON chunk for the handler to frame: %q", chunk)
		}
	}
	if string(out[3]) != input[3] {
		t.Fatalf("the final chunk changed: %q", out[3])
	}
}

func TestReplyStreamChatToolCallArgumentsAreNotText(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("secret"))
	call := `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"secret\"}"}}]}}]}`
	out, _ := runStream(t, contexts, sdktranslator.FormatOpenAI, call)
	if joined(out) != call {
		t.Fatalf("tool call arguments were rewritten: %q", joined(out))
	}
}

func TestReplyStreamResponsesDeltasAndTheFinalCopies(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	done := `event: response.output_text.done` + "\n" + `data: {"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"so badword here"}`
	completed := `event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","encrypted_content":"badword-opaque","summary":[]},{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"so badword here"}]}]}}`
	input := []string{responsesDelta("so bad"), responsesDelta("word h"), responsesDelta("ere"), done, completed}
	out, _ := runStream(t, contexts, sdktranslator.FormatOpenAIResponse, input...)
	if got := readerOf(sdktranslator.FormatOpenAIResponse, out); got != "so *** here" {
		t.Fatalf("the client reads %q", got)
	}
	if got := gjson.Get(docsOf(out[len(out)-2 : len(out)-1])[0], "text").String(); got != "so *** here" {
		t.Fatalf("the done event repeats the unfiltered text: %q", got)
	}
	final := docsOf(out[len(out)-1:])[0]
	if got := gjson.Get(final, "response.output.1.content.0.text").String(); got != "so *** here" {
		t.Fatalf("the completed event repeats the unfiltered text: %q", got)
	}
	if got := gjson.Get(final, "response.output.0.encrypted_content").String(); got != "badword-opaque" {
		t.Fatalf("encrypted reasoning was touched: %q", got)
	}
	// Responses events keep the shape the HTTP handler expects: no line ends added.
	for i, chunk := range out {
		if !bytes.HasPrefix(chunk, []byte("event: ")) || bytes.HasSuffix(chunk, []byte("\n")) {
			t.Fatalf("chunk %d changed shape: %q", i, chunk)
		}
	}
}

func TestReplyStreamGeminiChunks(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("badword"))
	for _, format := range []sdktranslator.Format{sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI} {
		wrap := func(doc string) string {
			if format == sdktranslator.FormatGeminiCLI {
				return `{"response":` + doc + `,"traceId":"t"}`
			}
			return doc
		}
		input := []string{wrap(geminiChunk("hello bad")), wrap(geminiChunk("word, bad")), wrap(geminiChunk("ly"))}
		out, _ := runStream(t, contexts, format, input...)
		if got := readerOf(format, out); got != "hello ***, badly" {
			t.Fatalf("%s: the client reads %q", format, got)
		}
		if len(out) != len(input) {
			t.Fatalf("%s: %d chunks out for %d in", format, len(out), len(input))
		}
		if !strings.Contains(string(out[0]), `"usageMetadata":{"totalTokenCount":3}`) {
			t.Fatalf("%s: metadata lost: %q", format, out[0])
		}
	}
}

func TestReplyStreamGeminiThoughtsAreNotTheAnswer(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, kw("secret"))
	doc := `{"candidates":[{"content":{"parts":[{"text":"a secret thought","thought":true}]},"index":0}]}`
	out, _ := runStream(t, contexts, sdktranslator.FormatGemini, doc)
	if joined(out) != doc {
		t.Fatalf("a thought summary was rewritten: %q", joined(out))
	}
}

func TestReplyStreamLogOnlyNeverHoldsOrChanges(t *testing.T) {
	contexts := replyContexts(t, WordActionLog, kw("badword"))
	engine := newReplyEngine(contexts, newWordGuardSnapshot(WordVerdict{}, nil, ""))
	stream := newWordReplyStream(engine, sdktranslator.FormatClaude)
	event := claudeText(0, "so bad")
	out := stream.Push([]byte(event))
	if len(out) != 1 || string(out[0]) != event {
		t.Fatalf("a log-only filter delayed or changed an event: %q", joined(out))
	}
	if _, held := stream.HeldSince(); held {
		t.Fatal("a log-only filter holds text")
	}
	more := stream.Push([]byte(claudeText(0, "word badword")))
	if readerOf(sdktranslator.FormatClaude, more) != "word badword" {
		t.Fatalf("got %q", joined(more))
	}
	if view := engine.snapshot.view.Reply; view == nil || view.Hits != 1 || view.Replaced != 0 {
		t.Fatalf("counters = %+v", view)
	}
}

func TestReplyStreamEscapesWhatItWrites(t *testing.T) {
	tenant := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
		p.Action, p.ReplyFilter, p.Replacement = WordActionReplace, boolPtr(true), `"[x]"`
	}, kw("bad"))
	contexts := buildWordContexts(nil, tenant, nil)
	text := "say \"bad\"\\ \n  😀 bad"
	out, _ := runStream(t, contexts, sdktranslator.FormatClaude, claudeText(0, text))
	want := "say \"\"[x]\"\"\\ \n  😀 \"[x]\""
	if got := readerOf(sdktranslator.FormatClaude, out); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The text a client reads must not depend on where the executor cut the reply
// into events, nor on how the events were cut into chunks.
func TestReplyStreamRandomCutsReadTheSame(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	words := []string{"alpha", "beta", "gamma", "badword", "bad", "word", "敏感词", "敏感", "x", "y", " ", "\n", ", ", "\"", "é"}
	engine := newReplyEngine(replyContexts(t, WordActionReplace, kw("badword"), kw("敏感词"), kw("alpha beta")), nil)
	contexts := replyContexts(t, WordActionReplace, kw("badword"), kw("敏感词"), kw("alpha beta"))
	for trial := 0; trial < 300; trial++ {
		var text strings.Builder
		for n := 2 + rng.IntN(25); n > 0; n-- {
			text.WriteString(words[rng.IntN(len(words))])
		}
		want := engine.filterText(text.String(), false).out
		var pieces []string
		for rest := text.String(); rest != ""; {
			n := 1 + rng.IntN(6)
			for n < len(rest) && !isRuneStart(rest[n]) {
				n++
			}
			n = min(n, len(rest))
			pieces = append(pieces, rest[:n])
			rest = rest[n:]
		}
		var events []string
		for _, piece := range pieces {
			events = append(events, claudeText(0, piece))
		}
		wire := claudeStart + claudeBlock + strings.Join(events, "") + claudeStop + claudeEnd
		// Cut the wire into chunks at line ends, at random.
		var chunks []string
		for rest := wire; rest != ""; {
			n := 1 + rng.IntN(4)
			end := 0
			for ; n > 0 && end < len(rest); n-- {
				i := strings.IndexByte(rest[end:], '\n')
				if i < 0 {
					end = len(rest)
					break
				}
				end += i + 1
			}
			chunks = append(chunks, rest[:end])
			rest = rest[end:]
		}
		out, _ := runStream(t, contexts, sdktranslator.FormatClaude, chunks...)
		if got := readerOf(sdktranslator.FormatClaude, out); got != want {
			t.Fatalf("pieces %q\nwant %q\ngot  %q", pieces, want, got)
		}
		if strings.Count(joined(out), "event: content_block_delta") != len(pieces) {
			t.Fatalf("event count changed for pieces %q", pieces)
		}
	}
}

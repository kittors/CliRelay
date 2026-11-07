package loopdetect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// stuckLines is a model going round the same few thoughts, a word or two
// changing from one round to the next: the shape of a reasoning loop.
var stuckLines = []string{
	"Okay.", "Let me think.", "Hmm.", "Proceeding.", "Alright.", "Next.", "Done?",
	"Okay.", "Let me think.", "Wait.", "Hmm.", "Proceeding.", "Alright.", "Continuing.",
}

// stuckText is n of stuckLines, cycling, each its own paragraph.
func stuckText(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(stuckLines[i%len(stuckLines)] + "\n\n")
	}
	return b.String()
}

// ordinaryReasoning is the reasoning a reply has before it loops, which is none.
const ordinaryReasoning = `The user wants the config loader to accept both YAML and TOML. Let me look at how parse() picks the format today: it switches on the extension, and an unknown one falls through to JSON.
So I need a case for ".toml" and a decoder for it. The repo already depends on a TOML library for the plugin manifests, so no new dependency.
Edge cases: an uppercase extension (".TOML"), a file with no extension, and a TOML file that fails to parse should report the line number.
I'll write the change now.
`

// pieces cuts s the way a vendor streams it: pieces a few characters long that
// end anywhere in a word or a line, never inside a character.
func pieces(s string, seed int64) []string {
	r := rand.New(rand.NewSource(seed))
	var out []string
	for len(s) > 0 {
		n := min(1+r.Intn(48), len(s))
		for n < len(s) && !utf8.RuneStart(s[n]) {
			n++
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return out
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// protocolChunks writes a delta of text in each client protocol, in the form the
// handler gets it: a Claude or Responses event whole, a Chat or Gemini object
// bare.
var protocolChunks = []struct {
	name      string
	reasoning bool
	chunk     func(s string) string
}{
	{"chat content", false, func(s string) string {
		return `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":` + quoteJSON(s) + `}}]}`
	}},
	{"chat reasoning_content", true, func(s string) string {
		return `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":` + quoteJSON(s) + `}}]}`
	}},
	{"chat reasoning", true, func(s string) string {
		return `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":` + quoteJSON(s) + `}}]}`
	}},
	{"claude text", false, func(s string) string {
		return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":" + quoteJSON(s) + "}}\n\n"
	}},
	{"claude thinking", true, func(s string) string {
		return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":" + quoteJSON(s) + "}}\n\n"
	}},
	{"responses text", false, func(s string) string {
		return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":" + quoteJSON(s) + "}\n\n"
	}},
	{"responses reasoning summary", true, func(s string) string {
		return "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"output_index\":0,\"summary_index\":0,\"delta\":" + quoteJSON(s) + "}\n\n"
	}},
	{"responses reasoning text", true, func(s string) string {
		return "data: {\"type\":\"response.reasoning_text.delta\",\"item_id\":\"rs_1\",\"delta\":" + quoteJSON(s) + "}\r\n\r\n"
	}},
	{"gemini text", false, func(s string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + quoteJSON(s) + `}]}}]}`
	}},
	{"gemini thought", true, func(s string) string {
		return `{"candidates":[{"content":{"role":"model","parts":[{"text":` + quoteJSON(s) + `,"thought":true}]}}]}`
	}},
	{"gemini cli text", false, func(s string) string {
		return `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":` + quoteJSON(s) + `}]}}]}}`
	}},
}

// feedAll feeds text to g in pieces, each written as chunk, and returns the trip.
func feedAll(g *ReplyGuard, chunk func(string) string, text string, seed int64) (ReplyTrip, bool) {
	for _, piece := range pieces(text, seed) {
		if trip, ok := g.Feed([]byte(chunk(piece))); ok {
			return trip, true
		}
	}
	return ReplyTrip{}, false
}

func chatText(s string) string {
	return protocolChunks[0].chunk(s)
}

func TestReplyGuardFindsALoopInEveryProtocol(t *testing.T) {
	for _, p := range protocolChunks {
		t.Run(p.name, func(t *testing.T) {
			g := NewReplyGuard(ReplyLimits{})
			if trip, ok := feedAll(g, p.chunk, ordinaryReasoning, 1); ok {
				t.Fatalf("ordinary writing was taken for a loop: %+v", trip)
			}
			trip, ok := feedAll(g, p.chunk, stuckText(600), 2)
			if !ok {
				t.Fatal("no loop found in 600 lines of one")
			}
			if trip.Reasoning != p.reasoning {
				t.Errorf("Reasoning = %v, want %v", trip.Reasoning, p.reasoning)
			}
			if trip.RunOn || trip.Window != DefaultReplyWindow || trip.Distinct > DefaultReplyDistinct {
				t.Errorf("trip = %+v", trip)
			}
			if trip.UnitsRead > DefaultReplyWindow+len(stuckLines) {
				t.Errorf("found only after %d units, the window is %d", trip.UnitsRead, DefaultReplyWindow)
			}
			if trip.Chunks == 0 || trip.Bytes == 0 {
				t.Errorf("the cut's place is not recorded: %+v", trip)
			}
		})
	}
}

func TestReplyGuardFindsALoopWhateverTheFormOfTheLines(t *testing.T) {
	for name, text := range map[string]string{
		"paragraphs":        stuckText(600),
		"one line":          strings.ReplaceAll(stuckText(600), "\n\n", " "),
		"windows newlines":  strings.ReplaceAll(stuckText(600), "\n", "\r\n"),
		"one word a line":   strings.Repeat("loop\nloop\nagain\nloop\nloop\nagain\n", 100),
		"chinese sentences": strings.Repeat("好。现在写。开始。让我写。好的。这里。输出。现在写。最终。开始。", 60),
		"no case":           strings.Repeat("Okay.\nOKAY.\nokay\nOkay!\n", 100),
	} {
		t.Run(name, func(t *testing.T) {
			g := NewReplyGuard(ReplyLimits{})
			if _, ok := feedAll(g, chatText, text, 3); !ok {
				t.Fatal("no loop found")
			}
		})
	}
}

// The unit counted is a line or a sentence, so a loop is told from a list whose
// items merely resemble one another.
func TestReplyGuardTellsALoopFromAListOfDifferentItems(t *testing.T) {
	var numbered, sentences strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&numbered, "%d. Check the item and mark it done.\n", i+1)
		fmt.Fprintf(&sentences, "Step %d of the plan is to check item %d. ", i, i*7)
	}
	for name, text := range map[string]string{
		"numbered list":     numbered.String(),
		"sentences by step": sentences.String(),
	} {
		g := NewReplyGuard(ReplyLimits{})
		if trip, ok := feedAll(g, chatText, text, 4); ok {
			t.Errorf("%s: taken for a loop: %s", name, trip.Message())
		}
	}
}

func TestReplyGuardReportsWhatLooped(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{})
	trip, ok := feedAll(g, protocolChunks[1].chunk, stuckText(600), 5)
	if !ok {
		t.Fatal("no loop found")
	}
	if !trip.Reasoning || len(trip.Units) == 0 || trip.Units[0] != "Okay." && trip.Units[0] != "Let me think." && trip.Units[0] != "Hmm." && trip.Units[0] != "Proceeding." && trip.Units[0] != "Alright." {
		t.Fatalf("most frequent unit first: %+v", trip)
	}
	counts := map[string]int{}
	for _, u := range trip.Units {
		counts[u]++
	}
	for u, n := range counts {
		if n != 1 {
			t.Errorf("unit %q listed %d times", u, n)
		}
	}
	msg := trip.Message()
	for _, want := range []string{`"Okay."`, "reasoning", "stuck in a loop", "256 lines and sentences", "Stop repeating replies", "Loop protection"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}

	// the answer's loop is the answer's
	g = NewReplyGuard(ReplyLimits{})
	trip, _ = feedAll(g, chatText, stuckText(600), 5)
	if trip.Reasoning || !strings.Contains(trip.Message(), "model's answer was stuck") {
		t.Errorf("answer trip: %+v / %s", trip, trip.Message())
	}
}

// A reply's reasoning and its answer are counted apart: a reply that alternates
// between two voices, each saying a few things over, has not looped in either
// voice's window until that voice has filled its own.
func TestReplyGuardCountsReasoningAndTextApart(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{})
	reason := protocolChunks[1].chunk
	for i := 0; i < 400; i++ {
		line := fmt.Sprintf("Considering option number %d for the design.\n", i)
		if trip, ok := g.Feed([]byte(reason(line))); ok {
			t.Fatalf("different lines tripped: %+v", trip)
		}
		if trip, ok := g.Feed([]byte(chatText("Thanks.\n"))); !ok {
			continue
		} else if trip.Reasoning {
			t.Fatalf("the loop of the answer was put down to the reasoning: %+v", trip)
		} else {
			return
		}
	}
	t.Fatal("the answer's own loop was not found")
}

// A tool call's arguments are no reply text: a tool writing a file of repeated
// lines is no loop of the model's.
func TestReplyGuardIgnoresToolArguments(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{})
	for _, piece := range pieces(stuckText(800), 6) {
		for _, ev := range []string{
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":" + quoteJSON(piece) + "}}\n\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":" + quoteJSON(piece) + "}\n\n",
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":` + quoteJSON(piece) + `}}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"write","args":{"text":` + quoteJSON(piece) + `}}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"content":null}}]}`,
		} {
			if trip, ok := g.Feed([]byte(ev)); ok {
				t.Fatalf("a call's arguments were read as a loop: %s\n%+v", ev, trip)
			}
		}
	}
}

// Events that carry no text, and bytes that are no event, are let by.
func TestReplyGuardLetsOtherEventsBy(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{})
	for _, chunk := range []string{
		"", "\n", ": keep-alive\n\n", "data: [DONE]\n\n", "[DONE]", "not json at all",
		`{"type":"message_start","message":{"id":"m1"}}`,
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n",
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n",
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":3}}`,
		`{"delta`, "data: {broken delta", `{"choices":"delta"}`,
		`{"type":"content_block_delta","delta":"text"}`,
	} {
		if trip, ok := g.Feed([]byte(chunk)); ok {
			t.Fatalf("%q tripped: %+v", chunk, trip)
		}
	}
}

// What the text of ordinary replies is made of is never taken for a loop, as
// reasoning or as the answer: source code, documents and their tables, a log, a
// numbered list, a CSV, a hex table, base64 and a run of rules and braces.
func TestReplyGuardLeavesOrdinaryTextAlone(t *testing.T) {
	texts := map[string]string{}
	for name, path := range repoSamples(t) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// a few hundred thousand characters are thousands of lines, many times
		// the window
		if len(b) > 256<<10 {
			b = b[:256<<10]
		}
		texts[name] = string(b)
	}
	var log, list, csv, hex, rules, table strings.Builder
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&log, "%s INFO gateway: POST /v1/messages 200 in %dms\n", at.Add(time.Duration(i)*1300*time.Millisecond).Format(time.RFC3339Nano), 80+i%400)
		fmt.Fprintf(&list, "%d. Check the item and mark it done.\n", i+1)
		fmt.Fprintf(&csv, "%d,ok,ok,true,%d\n", i, i*7%13)
		hex.WriteString("    0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0x00, 0x00,\n")
		rules.WriteString("}\n  }\n---\n| --- | --- |\n\n")
		fmt.Fprintf(&table, "| model-%d | ok | %d ms |\n", i%97, i)
	}
	raw := make([]byte, 60000)
	rand.New(rand.NewSource(1)).Read(raw)
	texts["log"], texts["list"], texts["csv"], texts["hex"], texts["rules"], texts["table"] =
		log.String(), list.String(), csv.String(), hex.String(), rules.String(), table.String()
	texts["base64"] = base64.StdEncoding.EncodeToString(raw)
	wrapped := base64.StdEncoding.EncodeToString(raw)
	for i := 76; i < len(wrapped); i += 77 {
		wrapped = wrapped[:i] + "\n" + wrapped[i:]
	}
	texts["base64 lines"] = wrapped
	// with no break at all, as a model writes a long paragraph, JSON or minified
	// code on one line: an unbroken loop is found by how few its words are
	for _, name := range []string{"CHANGELOG.md", "log", "csv", "hex", "rules", "table"} {
		if text, ok := texts[name]; ok {
			texts[name+" on one line"] = strings.Join(strings.Fields(text), " ")
		}
	}
	var records strings.Builder
	records.WriteString("[")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&records, `{"id":%d,"type":"file","done":false},`, i)
	}
	texts["json records"] = records.String() + "]"
	// zeros are a long run of one letter in base64
	texts["base64 of zeros"] = base64.StdEncoding.EncodeToString(make([]byte, 30000))

	names := make([]string, 0, len(texts))
	for name := range texts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, p := range []struct {
			field string
			chunk func(string) string
		}{{"reasoning", protocolChunks[1].chunk}, {"text", chatText}} {
			g := NewReplyGuard(ReplyLimits{})
			if trip, ok := feedAll(g, p.chunk, texts[name], 7); ok {
				t.Errorf("%s as %s: taken for a loop: %s", name, p.field, trip.Message())
			}
		}
	}
}

// repoSamples are real documents and source of this repository, the writing a
// model produces most: the changelog, the readme and the biggest Go files.
func repoSamples(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	samples := map[string]string{}
	for _, name := range []string{"CHANGELOG.md", "README.md", "README_CN.md", "go.sum"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			samples[name] = filepath.Join(root, name)
		}
	}
	for _, pattern := range []string{"sdk/api/handlers/*.go", "sdk/cliproxy/auth/*.go", "internal/usage/*.go"} {
		files, _ := filepath.Glob(filepath.Join(root, pattern))
		sort.Slice(files, func(i, j int) bool {
			a, _ := os.Stat(files[i])
			b, _ := os.Stat(files[j])
			if a == nil || b == nil {
				return files[i] < files[j]
			}
			return a.Size() > b.Size()
		})
		for _, file := range files[:min(len(files), 3)] {
			samples[filepath.ToSlash(file[len(root)+1:])] = file
		}
	}
	if len(samples) < 5 {
		t.Fatalf("only %d samples of the repository found", len(samples))
	}
	return samples
}

// A loop with no break in it (a line or sentence's end) is found too, in the
// reasoning or the text, within some 2,000 tokens of it: run on, a unit is cut
// only every maxUnitBytes, and 256 of them would be far past any reply's limit.
func TestReplyGuardFindsAnUnbrokenLoop(t *testing.T) {
	before := "Let me look at the MCP servers configured for this project and pick the one that lists files"
	for name, loop := range map[string]string{
		"spaced":      "mcp ",
		"commas":      "mcp, MCP, ",
		"no space":    "mcp",
		"a few words": "call the mcp tool then call the mcp ",
		"chinese":     "重复说mcp",
	} {
		for _, p := range []struct {
			field string
			chunk func(string) string
			think bool
		}{{"text", chatText, false}, {"reasoning", protocolChunks[1].chunk, true}} {
			g := NewReplyGuard(ReplyLimits{})
			trip, ok := feedAll(g, p.chunk, before+" "+strings.Repeat(loop, 20000/len(loop)), 8)
			if !ok || !trip.RunOn || trip.Reasoning != p.think {
				t.Errorf("%s as %s: %+v %v", name, p.field, trip, ok)
				continue
			}
			msg := trip.Message()
			if !strings.Contains(msg, "mcp") || !strings.Contains(msg, "no break") || !strings.Contains(msg, "Stop repeating replies") {
				t.Errorf("%s: message %q", name, msg)
			}
			// Two blocks of it make the loop, one block of the same words is
			// not yet: it is the second that tells a loop from a long phrase.
			if trip.Window != 2*maxUnitBytes {
				t.Errorf("%s: found after %d characters of it, want %d", name, trip.Window, 2*maxUnitBytes)
			}
		}
	}
	// broken into lines longer than a unit, it is still found
	g := NewReplyGuard(ReplyLimits{})
	if _, ok := feedAll(g, chatText, strings.Repeat(strings.Repeat("mcp ", 1500)+"\n", 4), 9); !ok {
		t.Error("an unbroken loop in long lines was not found")
	}
	// but a lone character repeated is not a loop of words
	g = NewReplyGuard(ReplyLimits{})
	if trip, ok := feedAll(g, chatText, strings.Repeat("a", 20000), 10); ok {
		t.Errorf("a row of one letter was taken for a loop: %+v", trip)
	}
}

// differentLines are n lines no two of which are the same sentence.
func differentLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("Note %s again.", strings.Repeat(string(rune('a'+i%26)), 1+i/26))
	}
	return lines
}

// cycleLines writes lines rounds times over, each on a line of its own.
func cycleLines(lines []string, rounds int) string {
	var b strings.Builder
	for i := 0; i < rounds; i++ {
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// A window is a loop when it holds no more than Distinct different units: one
// with exactly that many is, and one with a single unit more is writing.
func TestReplyGuardDistinctLimitIsInclusive(t *testing.T) {
	for _, limits := range []ReplyLimits{
		{Window: 64, Distinct: 2}, {Window: 64, Distinct: 8}, {Window: 64, Distinct: 16},
		{Window: 256, Distinct: 24}, {Window: 256, Distinct: 64},
	} {
		t.Run(fmt.Sprintf("window %d distinct %d", limits.Window, limits.Distinct), func(t *testing.T) {
			g := NewReplyGuard(limits)
			trip, ok := feedAll(g, chatText, cycleLines(differentLines(limits.Distinct), 4*limits.Window), 21)
			if !ok || trip.Distinct != limits.Distinct {
				t.Errorf("a window of exactly %d different units was not taken for a loop: %+v %v", limits.Distinct, trip, ok)
			}
			g = NewReplyGuard(limits)
			if trip, ok := feedAll(g, chatText, cycleLines(differentLines(limits.Distinct+1), 4*limits.Window), 21); ok {
				t.Errorf("a window of %d different units was taken for a loop at a limit of %d: %+v", limits.Distinct+1, limits.Distinct, trip)
			}
		})
	}
}

// A window is looked at once it is full. A model that says one thing a few
// times has not looped, however few different units it has said.
func TestReplyGuardWaitsForAFullWindow(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{Window: 64, Distinct: 4})
	for i := 1; i < 64; i++ {
		if trip, ok := g.Feed([]byte(chatText("Okay.\n"))); ok {
			t.Fatalf("a loop after %d units, the window is 64: %+v", i, trip)
		}
	}
	trip, ok := g.Feed([]byte(chatText("Okay.\n")))
	if !ok || trip.UnitsRead != 64 || trip.Window != 64 {
		t.Fatalf("the 64th unit filled the window and it was not a loop: %+v %v", trip, ok)
	}
}

// Units are told apart by their words. The same sentence in another case, or
// with another closing stop, is that sentence said again.
func TestReplyGuardTakesCaseAndClosingStopsForTheSameUnit(t *testing.T) {
	// two different units is the least a limit allows, so a window of more is
	// not a loop
	limits := ReplyLimits{Window: 64, Distinct: 2}
	for name, lines := range map[string][]string{
		"case": {"Okay, running it again.", "OKAY, RUNNING IT AGAIN.", "okay, running it again.", "Okay, Running It Again."},
		"stops": {
			"Okay, running it again.", "Okay, running it again!", "Okay, running it again?", "Okay, running it again,",
			"Okay, running it again;", "Okay, running it again:", "Okay, running it again…", "Okay, running it again。",
			"Okay, running it again",
		},
	} {
		g := NewReplyGuard(limits)
		trip, ok := feedAll(g, chatText, cycleLines(lines, 100), 22)
		if !ok || trip.Distinct != 1 {
			t.Errorf("%s: %d spellings of one sentence were not read as one unit: %+v %v", name, len(lines), trip, ok)
		}
	}
}

// What leaves the window is forgotten. A model that wrote on for a long while
// and then got stuck is found once the loop has filled the window, not never.
func TestReplyGuardForgetsUnitsThatLeaveTheWindow(t *testing.T) {
	var before strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&before, "Considering option number %d for the design.\n", i)
	}
	g := NewReplyGuard(ReplyLimits{})
	if trip, ok := feedAll(g, chatText, before.String(), 23); ok {
		t.Fatalf("300 different lines were taken for a loop: %+v", trip)
	}
	trip, ok := feedAll(g, chatText, stuckText(600), 24)
	if !ok {
		t.Fatal("a loop that began after 300 different lines was not found")
	}
	// the window held 256 of the old lines when the loop began
	if trip.UnitsRead > 300+DefaultReplyWindow+len(stuckLines) || trip.Distinct > DefaultReplyDistinct {
		t.Errorf("found only after %d units, with %d different in the window: %+v", trip.UnitsRead, trip.Distinct, trip)
	}
}

// A few long words with no break are no loop of words: they are an identifier,
// a token or a hash, which is what the check that finds "mcp mcp mcp …" must
// not mistake for one, however few the different words are.
func TestReplyGuardLeavesFewLongWordsAlone(t *testing.T) {
	rnd := rand.New(rand.NewSource(25))
	word := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + rnd.Intn(26))
		}
		return string(b)
	}
	for name, text := range map[string]string{
		"one word":    word(20000),
		"a few words": strings.Join([]string{word(3000), word(3000), word(3000), word(3000), word(3000), word(3000)}, " "),
	} {
		g := NewReplyGuard(ReplyLimits{})
		if trip, ok := feedAll(g, chatText, text, 26); ok {
			t.Errorf("%s: taken for a loop: %+v", name, trip)
		}
	}
}

func TestReplyLimitsAreHeldToTheirBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ReplyLimits
		want ReplyLimits
	}{
		{"unset", ReplyLimits{}, ReplyLimits{Window: 256, Distinct: 24}},
		{"negative", ReplyLimits{Window: -5, Distinct: -1}, ReplyLimits{Window: 256, Distinct: 24}},
		{"window too small", ReplyLimits{Window: 3, Distinct: 2}, ReplyLimits{Window: 64, Distinct: 2}},
		{"window too big", ReplyLimits{Window: 1 << 20, Distinct: 24}, ReplyLimits{Window: 2048, Distinct: 24}},
		{"distinct too big", ReplyLimits{Window: 256, Distinct: 200}, ReplyLimits{Window: 256, Distinct: 64}},
		{"distinct as big as it may be", ReplyLimits{Window: 64, Distinct: 16}, ReplyLimits{Window: 64, Distinct: 16}},
		{"distinct under the floor", ReplyLimits{Window: 256, Distinct: 1}, ReplyLimits{Window: 256, Distinct: 2}},
		{"default distinct in a small window", ReplyLimits{Window: 64}, ReplyLimits{Window: 64, Distinct: 16}},
		{"in bounds", ReplyLimits{Window: 128, Distinct: 10}, ReplyLimits{Window: 128, Distinct: 10}},
	} {
		if got := tc.in.Normalized(); got != tc.want {
			t.Errorf("%s: Normalized(%+v) = %+v, want %+v", tc.name, tc.in, got, tc.want)
		}
	}
}

// A smaller window is found sooner, and a window's own thresholds are the ones
// reported and used.
func TestReplyGuardUsesItsOwnLimits(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{Window: 64, Distinct: 10})
	if got := g.Limits(); got != (ReplyLimits{Window: 64, Distinct: 10}) {
		t.Fatalf("limits %+v", got)
	}
	trip, ok := feedAll(g, chatText, stuckText(600), 11)
	if !ok || trip.Window != 64 || trip.UnitsRead > 64+len(stuckLines) {
		t.Fatalf("trip %+v %v", trip, ok)
	}
	// the stuck lines are 9 different ones: a limit under that never trips on them
	g = NewReplyGuard(ReplyLimits{Window: 64, Distinct: 8})
	if trip, ok := feedAll(g, chatText, stuckText(600), 11); ok {
		t.Fatalf("9 different lines tripped a limit of 8: %+v", trip)
	}
	// 13 different lines in a window of 64 are more than 8 different ones
	g = NewReplyGuard(ReplyLimits{Window: 64, Distinct: 8})
	if trip, ok := feedAll(g, chatText, strings.Repeat(strings.Join([]string{
		"alpha one.", "beta two.", "gamma three.", "delta four.", "epsilon five.", "zeta six.", "eta seven.", "theta eight.", "iota nine.", "kappa ten.", "lambda eleven.", "mu twelve.", "nu thirteen.",
	}, "\n")+"\n", 30), 12); ok {
		t.Fatalf("a window with more different units than the limit tripped: %+v", trip)
	}
}

func TestReplyLoopErrorReadsAsAnOpenAIError(t *testing.T) {
	g := NewReplyGuard(ReplyLimits{})
	trip, ok := feedAll(g, chatText, stuckText(600), 13)
	if !ok {
		t.Fatal("no loop found")
	}
	err := &ReplyLoopError{Trip: trip}
	if err.StatusCode() != 422 {
		t.Errorf("status %d", err.StatusCode())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if e := json.Unmarshal([]byte(err.Error()), &body); e != nil {
		t.Fatalf("not JSON: %v: %s", e, err.Error())
	}
	if body.Error.Code != ReplyLoopCode || body.Error.Type != "invalid_request_error" || body.Error.Message != trip.Message() {
		t.Errorf("body %+v", body)
	}
	// the words of an over-long prompt would make the handlers rewrite the error
	// for a client that compacts its conversation
	for _, word := range []string{"context length", "context window", "too long", "token"} {
		if strings.Contains(strings.ToLower(trip.Message()), word) {
			t.Errorf("the message says %q", word)
		}
	}
}

func BenchmarkReplyGuardFeed(b *testing.B) {
	text := ordinaryReasoning + strings.Repeat("The function returns an error when the input is empty, and nil otherwise.\n", 3)
	chunks := make([][]byte, 0, 64)
	for _, piece := range pieces(text, 14) {
		chunks = append(chunks, []byte(chatText(piece)))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := NewReplyGuard(ReplyLimits{})
		for _, c := range chunks {
			g.Feed(c)
		}
	}
}

package contentmoderation

import (
	"math/rand/v2"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// RE2 cannot say whether the end of a text is the start of a match. For an
// expression whose matches have a longest length it does not have to: the start
// of a match is anything it can begin with in the last (longest - 1)
// characters. The first digits of a phone number are then held back until the
// rest of it shows, instead of going out ten at a time.

func streamedOneRuneAtATime(engine *replyEngine, text string) string {
	var pieces []string
	for _, r := range text {
		pieces = append(pieces, string(r))
	}
	return streamText(engine, pieces...)
}

func TestReplyStreamHoldsBackTheStartOfABoundedRegex(t *testing.T) {
	engine, snapshot := replyEngineOf(t, WordActionReplace, rx(`\d{11}`))
	text := "call 13800138000 now, or 1380013800 which is short"
	want := "call *** now, or 1380013800 which is short"
	if got := engine.filterText(text, false).out; got != want {
		t.Fatalf("whole text: %q", got)
	}
	if got := streamedOneRuneAtATime(engine, text); got != want {
		t.Fatalf("one character at a time:\n got  %q\n want %q", got, want)
	}
	if view := snapshot.view.Reply; view == nil || view.Replaced < 2 {
		t.Fatalf("counters = %+v", view)
	}

	// The same number in the cuts a network makes.
	for _, cuts := range [][]string{
		{"call 138", "00138", "000 now"},
		{"call 1", "3800138000", " now"},
		{"call 13800138000", " now"},
		{"call ", "1380013800", "0"},
	} {
		fresh, _ := replyEngineOf(t, WordActionReplace, rx(`\d{11}`))
		whole := strings.Join(cuts, "")
		if got, want := streamText(fresh, cuts...), fresh.filterText(whole, false).out; got != want {
			t.Fatalf("cuts %q: got %q, want %q", cuts, got, want)
		}
	}
}

// Text that cannot begin a match is not held at all, so a reply does not lag by
// the length of the longest expression.
func TestReplyStreamOnlyHoldsWhatCouldBeginAMatch(t *testing.T) {
	engine, _ := replyEngineOf(t, WordActionReplace, rx(`\d{11}`), rx(`foo.{2,40}bar`))
	for _, text := range []string{"hello world, nothing to see", "ordinary words 12 and 345"} {
		r := engine.filterText(text, true)
		// A digit near the end could begin an 11-digit number; plain words could
		// not, and nothing is held for longer than the number can reach.
		if strings.ContainsAny(text, "0123456789") {
			if r.out+r.held != text || r.out == "" || r.held == "" || len(r.held) > len("12 and 345") {
				t.Fatalf("%q: out %q held %q", text, r.out, r.held)
			}
			continue
		}
		if r.out != text || r.held != "" {
			t.Fatalf("%q: out %q held %q, want it all out", text, r.out, r.held)
		}
	}
	// A character that can begin the expression starts the hold.
	if r := engine.filterText("so fo", true); r.out != "so " || r.held != "fo" {
		t.Fatalf("got %+v", r)
	}
	// And the hold is no longer than the expression can reach.
	long := strings.Repeat("foo", 60)
	if r := engine.filterText(long, true); len(long)-len(r.held) < len(long)-(3+40+3) {
		t.Fatalf("held %d bytes of %d", len(r.held), len(long))
	}
}

// An expression that can match any length cannot be held back that way, and the
// reply filter holds back only a match it can see. This pins the limit down: it
// is what the panel tells the user about such expressions.
func TestReplyStreamCannotHoldBackTheStartOfAnUnboundedRegex(t *testing.T) {
	for _, pattern := range []string{`\d{11,}`, `\d+x`} {
		engine, _ := replyEngineOf(t, WordActionReplace, rx(pattern))
		r := engine.filterText("call 138", true)
		if r.out != "call 138" || r.held != "" {
			t.Fatalf("%s: got %+v; the start of an expression without a longest match is not held", pattern, r)
		}
	}
	// A match that is whole in the text is replaced, and one that runs to the end
	// of what has arrived waits to see whether it goes on.
	engine, _ := replyEngineOf(t, WordActionReplace, rx(`\d{11,}`))
	if r := engine.filterText("call 13800138000", true); r.out != "call " || r.held != "13800138000" {
		t.Fatalf("got %+v", r)
	}
	if got := streamText(engine, "call 13800138000", "123 now"); got != "call *** now" {
		t.Fatalf("got %q", got)
	}
}

func TestWordRegexIsBounded(t *testing.T) {
	tests := map[string]bool{
		`\d{11}`:          true,
		`1[3-9]\d{9}`:     true,
		`foo.{2,5}bar`:    true,
		`(?:ab|cde){2}`:   true,
		`colou?r`:         true,
		`a|bb|ccc`:        true,
		`\d+`:             false,
		`\d{11,}`:         false,
		`foo.*bar`:        false,
		`(?:ab)+`:         false,
		`[a-z]{3}@[a-z]+`: false,
		`(unterminated`:   false,
		``:                false,
		`a{2}(?:b|c+)`:    false,
		`\bword\b`:        true,
		`(?i)secret\d{4}`: true,
	}
	for pattern, want := range tests {
		if got := WordRegexIsBounded(pattern); got != want {
			t.Errorf("WordRegexIsBounded(%q) = %v, want %v", pattern, got, want)
		}
	}
}

// Whatever the cuts, what a client reads of a stream is what the whole text
// would have read after filtering, for expressions with a longest match, mixed
// with keywords.
func TestReplyStreamBoundedRegexReadsTheSameWhateverTheCuts(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32))
	rules := []WordRule{
		rx(`\d{11}`), rx(`1[3-9]\d{9}`), rx(`[a-z]{2,4}@x\.com`), rx(`foo.{2,5}bar`), rx(`(?:ab|cde){2}`),
		kw("badword"), kw("敏感词"),
	}
	pieces := []string{"1", "3", "8", "0", "0", "9", "12", "1380013800", "abc", "ab", "cde", "x", "@", ".", "com", "foo", "bar", "q", " ", "\n", "bad", "word", "敏感", "词", "é", "ab cde"}
	for trial := 0; trial < 600; trial++ {
		engine, _ := replyEngineOf(t, WordActionReplace, rules...)
		var text strings.Builder
		for n := 3 + rng.IntN(30); n > 0; n-- {
			text.WriteString(pieces[rng.IntN(len(pieces))])
		}
		whole := engine.filterText(text.String(), false).out
		var cuts []string
		for rest := text.String(); rest != ""; {
			n := 1 + rng.IntN(7)
			for n < len(rest) && !isRuneStart(rest[n]) {
				n++
			}
			n = min(n, len(rest))
			cuts = append(cuts, rest[:n])
			rest = rest[n:]
		}
		fresh, _ := replyEngineOf(t, WordActionReplace, rules...)
		if got := streamText(fresh, cuts...); got != whole {
			t.Fatalf("trial %d, cuts %q\n got  %q\n want %q", trial, cuts, got, whole)
		}
	}
}

// The same through the protocol codecs: the number is split over events.
func TestReplyStreamRegexAcrossClaudeEvents(t *testing.T) {
	contexts := replyContexts(t, WordActionReplace, rx(`\d{11}`))
	input := []string{claudeStart, claudeBlock}
	for _, r := range "call 13800138000 now" {
		input = append(input, claudeText(0, string(r)))
	}
	input = append(input, claudeStop, claudeEnd)
	out, snapshot := runStream(t, contexts, sdktranslator.FormatClaude, input...)
	if got := readerOf(sdktranslator.FormatClaude, out); got != "call *** now" {
		t.Fatalf("the client reads %q", got)
	}
	deltas := 0
	for _, doc := range docsOf(out) {
		if gjson.Get(doc, "type").String() == "content_block_delta" {
			deltas++
		}
	}
	if deltas != len([]rune("call 13800138000 now")) {
		t.Fatalf("%d delta events out, one per event in expected", deltas)
	}
	if view := snapshot.view.Reply; view == nil || view.Replaced != 1 {
		t.Fatalf("counters = %+v", view)
	}
}

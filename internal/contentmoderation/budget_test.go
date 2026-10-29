package contentmoderation

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitTailAlignsChunksToTheEnd(t *testing.T) {
	if got := splitTail("abcdefghij", 4); !reflect.DeepEqual(got, []string{"ab", "cdef", "ghij"}) {
		t.Fatalf("chunks = %q", got)
	}
	// Multi-byte runes must not be split mid-character.
	if got := splitTail(strings.Repeat("中", 10), 4); !reflect.DeepEqual(got, []string{"中中", "中中中中", "中中中中"}) {
		t.Fatalf("chunks = %q", got)
	}
	if got := splitTail("", 4); len(got) != 0 {
		t.Fatalf("empty text produced %q", got)
	}
}

func TestLastRunes(t *testing.T) {
	if got := lastRunes("héllo", 4); got != "éllo" {
		t.Fatalf("lastRunes = %q", got)
	}
	if got := lastRunes("hi", 5); got != "hi" {
		t.Fatalf("lastRunes = %q", got)
	}
	if got := lastRunes("hi", 0); got != "" {
		t.Fatalf("lastRunes = %q", got)
	}
}

func TestSplitNewestUserMessage(t *testing.T) {
	segments := []Segment{
		{Class: ClassPrimary, Turn: 0, Text: "system"},
		{Class: ClassPrimary, User: true, Turn: 1, Text: "old question"},
		{Class: ClassAssistantHistory, Turn: 2, Text: "answer"},
		{Class: ClassToolOutput, Turn: 3, Text: "tool result"},
		{Class: ClassPrimary, User: true, Turn: 3, Text: "new"},
		{Class: ClassPrimary, User: true, Turn: 3, Text: "question"},
		// A developer note after the newest user turn does not replace it.
		{Class: ClassPrimary, Turn: 4, Text: "developer note"},
	}
	newest, rest := splitNewestUserMessage(segments)
	if !reflect.DeepEqual(newest, []int{4, 5}) || !reflect.DeepEqual(rest, []int{0, 1, 2, 3, 6}) {
		t.Fatalf("newest = %v rest = %v", newest, rest)
	}
	// Without a user turn the newest primary message stands in.
	newest, rest = splitNewestUserMessage([]Segment{{Class: ClassPrimary, Turn: 0, Text: "system"}, {Class: ClassToolOutput, Turn: 1, Text: "x"}})
	if !reflect.DeepEqual(newest, []int{0}) || !reflect.DeepEqual(rest, []int{1}) {
		t.Fatalf("newest = %v rest = %v", newest, rest)
	}
	newest, rest = splitNewestUserMessage([]Segment{{Class: ClassToolOutput, Turn: 0, Text: "x"}})
	if len(newest) != 0 || !reflect.DeepEqual(rest, []int{0}) {
		t.Fatalf("newest = %v rest = %v", newest, rest)
	}
}

func TestKeepNewestSkipsTheOldestAndCountsPerClass(t *testing.T) {
	segments := []Segment{
		{Class: ClassPrimary, Text: strings.Repeat("p", 10)},
		{Class: ClassToolOutput, Text: strings.Repeat("t", 10)},
		{Class: ClassPrimary, Text: strings.Repeat("n", 10)},
	}
	var skipped skippedChars
	kept := keepNewest(segments, []int{0, 1, 2}, 15, &skipped)
	// The newest segment whole, one separator, then the last 4 characters of the next.
	if joinPieces(kept) != "tttt nnnnnnnnnn" {
		t.Fatalf("kept = %q", joinPieces(kept))
	}
	if kept[0].index != 1 || kept[1].index != 2 {
		t.Fatalf("kept pieces out of order: %+v", kept)
	}
	if skipped[ClassToolOutput] != 6 || skipped[ClassPrimary] != 10 || skipped[ClassAssistantHistory] != 0 {
		t.Fatalf("skipped = %v", skipped)
	}

	skipped = skippedChars{}
	if kept = keepNewest(segments, []int{0, 1, 2}, 100, &skipped); joinPieces(kept) != "pppppppppp tttttttttt nnnnnnnnnn" || skipped.total() != 0 {
		t.Fatalf("kept = %q skipped = %v", joinPieces(kept), skipped)
	}
	skipped = skippedChars{}
	if kept = keepNewest(segments, []int{0, 1, 2}, 0, &skipped); len(kept) != 0 || skipped.total() != 30 {
		t.Fatalf("kept = %q skipped = %v", joinPieces(kept), skipped)
	}
}

// overlongConversation is a coding-agent request: a system prompt far larger
// than any scan budget, a long history, and a short final question.
func overlongConversation() []Segment {
	segments := []Segment{{Class: ClassPrimary, Turn: 0, Text: strings.Repeat("system prompt ", 4000)}}
	for turn := 1; turn <= 20; turn++ {
		segments = append(segments,
			Segment{Class: ClassPrimary, User: true, Turn: turn * 2, Text: strings.Repeat("earlier question ", 50)},
			Segment{Class: ClassToolOutput, Turn: turn * 2, Text: strings.Repeat("tool output ", 300)},
		)
	}
	return append(segments, Segment{Class: ClassPrimary, User: true, Turn: 99, Text: "FINAL QUESTION"})
}

func TestSelectNewestTextKeepsTheNewestUserMessage(t *testing.T) {
	segments := overlongConversation()
	text, skipped := selectNewestText(segments, 1000)
	if utf8.RuneCountInString(text) > 1000 {
		t.Fatalf("selected %d characters, budget 1000", utf8.RuneCountInString(text))
	}
	if !strings.HasSuffix(text, "FINAL QUESTION") {
		t.Fatalf("selected text does not end with the newest user message: ...%q", text[len(text)-40:])
	}
	if skipped[ClassPrimary] == 0 || skipped[ClassToolOutput] == 0 {
		t.Fatalf("skipped = %v, want both classes counted", skipped)
	}
}

// Text newer than the newest user message (here, tool output that follows it)
// keeps its place after it: the newest message is kept first, not moved last.
func TestSelectNewestTextKeepsConversationOrder(t *testing.T) {
	segments := []Segment{
		{Class: ClassPrimary, Turn: 0, Text: "system"},
		{Class: ClassPrimary, User: true, Turn: 1, Text: "question"},
		{Class: ClassToolOutput, Turn: 2, Text: "result"},
	}
	if text, _ := selectNewestText(segments, 100); text != "system question result" {
		t.Fatalf("text = %q", text)
	}
	// With room for the question alone, it wins over the newer tool output.
	if text, skipped := selectNewestText(segments, 9); text != "question" || skipped[ClassToolOutput] != 6 || skipped[ClassPrimary] != 6 {
		t.Fatalf("text = %q skipped = %v", text, skipped)
	}
}

func TestGuardChunksScanTheNewestUserMessageFirst(t *testing.T) {
	segments := overlongConversation()
	chunks, skipped := guardChunks(segments, 200, 4)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4", len(chunks))
	}
	if chunks[0] != "FINAL QUESTION" {
		t.Fatalf("first chunk = %q, want the newest user message on its own", chunks[0])
	}
	total := 0
	for _, chunk := range chunks {
		if n := utf8.RuneCountInString(chunk); n > 200 {
			t.Fatalf("chunk of %d characters exceeds the limit", n)
		}
		total += utf8.RuneCountInString(chunk)
	}
	all := 0
	for _, segment := range segments {
		all += utf8.RuneCountInString(segment.Text)
	}
	// Every character is either scanned or reported as skipped (separators aside).
	if total+skipped.total() < all || skipped.total() == 0 {
		t.Fatalf("scanned %d + skipped %d does not cover %d characters", total, skipped.total(), all)
	}
}

func TestGuardChunksSplitAnOverlongNewestMessageFromItsEnd(t *testing.T) {
	segments := []Segment{
		{Class: ClassPrimary, Turn: 0, Text: "system"},
		{Class: ClassPrimary, User: true, Turn: 1, Text: strings.Repeat("a", 500) + "TAIL"},
	}
	chunks, skipped := guardChunks(segments, 128, 2)
	if len(chunks) != 2 || !strings.HasSuffix(chunks[0], "TAIL") || utf8.RuneCountInString(chunks[0]) != 128 {
		t.Fatalf("chunks = %q", chunks)
	}
	// The message filled the budget: its head and the system prompt are skipped.
	if skipped[ClassPrimary] != 504-256+len("system") {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestGuardChunksFitShortRequestsWhole(t *testing.T) {
	segments := []Segment{
		{Class: ClassPrimary, Turn: 0, Text: "system"},
		{Class: ClassPrimary, User: true, Turn: 1, Text: "hello"},
	}
	chunks, skipped := guardChunks(segments, DefaultInputLimit, DefaultMaxChunks)
	if !reflect.DeepEqual(chunks, []string{"hello", "system"}) || skipped.total() != 0 {
		t.Fatalf("chunks = %q skipped = %v", chunks, skipped)
	}
}

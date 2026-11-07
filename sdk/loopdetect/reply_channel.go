package loopdetect

import (
	"hash/fnv"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Thresholds of the repeated-reply check. They are what a Policy that does not
// set its own gets, and the bounds a Policy's own are held to.
const (
	// DefaultReplyWindow is how many of a channel's latest units are looked at.
	// 256 lines of a loop are some 600 tokens, a moment of a reply that runs to
	// an output limit of tens of thousands.
	DefaultReplyWindow = 256
	// DefaultReplyDistinct is the most different units a window may hold and
	// still be a loop. Ordinary writing never comes near it, even when lines
	// repeat: code, a table, a list or a log differs from line to line in its
	// names, numbers and cells.
	DefaultReplyDistinct = 24

	MinReplyWindow   = 64
	MaxReplyWindow   = 2048
	MinReplyDistinct = 2
	// MaxDistinctShare keeps a raised Distinct from reading ordinary text as a
	// loop: at most a quarter of the window may be different units.
	MaxDistinctShare = 4
)

const (
	// maxUnitBytes is how long a unit grows with no break in it before it is
	// judged on its own (see add): a loop with no line or sentence in it is
	// never 256 units before the reply's limit.
	maxUnitBytes = 4096
	// keyBytes is how much of a unit tells it apart from another.
	keyBytes = 256
	// quoteRunes bounds a quoted unit in an error and in the diagnostics.
	quoteRunes = 40
	// maxQuoted is how many different units an error quotes.
	maxQuoted = 6
	// runOnBlocks is how many unbroken blocks of maxUnitBytes in a row, each the
	// same few words, are a loop: some 2,000 tokens of it.
	runOnBlocks = 2
	// runOnDistinct is the most different words an unbroken loop is made of.
	runOnDistinct = 8
	// runOnMaxPeriod is the longest piece an unbroken loop repeats as one.
	runOnMaxPeriod = 64
	// runOnLongLine: a unit this long that still is a few words keeps the
	// unbroken count when a newline ends it (a loop broken into long lines).
	runOnLongLine = maxUnitBytes / 8
)

// ReplyLimits are the thresholds of the repeated-reply check: of a channel's
// last Window units, no more than Distinct different ones make a loop.
type ReplyLimits struct {
	Window   int `json:"window"`
	Distinct int `json:"max_distinct"`
}

// Normalized returns l with unset fields defaulted and every field held to its
// bounds, so the check never runs with a threshold that reads normal text as a
// loop or never trips.
func (l ReplyLimits) Normalized() ReplyLimits {
	if l.Window <= 0 {
		l.Window = DefaultReplyWindow
	}
	l.Window = min(max(l.Window, MinReplyWindow), MaxReplyWindow)
	if l.Distinct <= 0 {
		l.Distinct = DefaultReplyDistinct
	}
	l.Distinct = min(max(l.Distinct, MinReplyDistinct), l.Window/MaxDistinctShare)
	return l
}

// ReplyTrip is what the check found: which channel looped, and on what.
type ReplyTrip struct {
	// Reasoning: the model's reasoning looped, not its answer.
	Reasoning bool
	// RunOn: the loop had no line or sentence break in it. Units are then its
	// words and Window how many characters it ran to.
	RunOn bool
	// Units are the different units of the window, the most frequent first,
	// each cut short to be quoted.
	Units []string
	// Window is how many units were looked at; Distinct how many different
	// ones they held.
	Window   int
	Distinct int
	// UnitsRead is how many units the channel had counted when it tripped, and
	// Chunks and Bytes how much of the reply the guard had been fed: where in
	// the reply the cut was made.
	UnitsRead int
	Chunks    int
	Bytes     int64
}

// unitStat counts one different unit of the window.
type unitStat struct {
	n    int
	text string // as first written, for an error to quote
	at   int    // when it first came, to quote ties in order
}

// channel is one stream of text of a reply: its reasoning, or its answer.
type channel struct {
	cur []byte // the unit being written
	// stop: cur ends in a sentence's stop, so a space after it ends the unit.
	stop bool
	// lettered: cur has a letter in it, for a stop after it to end a sentence
	// rather than be a list's number ("1.") or a decimal.
	lettered bool
	ring     []uint64
	counts   map[uint64]*unitStat
	seen     int // units counted so far
	// runOn is how many unbroken blocks in a row were the same few words.
	runOn int
}

// add reads s, the next of the channel, and says whether it became a loop.
func (c *channel) add(s string, lim ReplyLimits) (ReplyTrip, bool) {
	for _, r := range s {
		switch {
		case r == '\n':
			if trip, ok := c.end(lim); ok {
				return trip, true
			}
			continue
		case c.stop && unicode.IsSpace(r):
			if trip, ok := c.end(lim); ok {
				return trip, true
			}
			continue
		}
		c.cur = utf8.AppendRune(c.cur, r)
		switch r {
		case '.', '!', '?':
			c.stop = c.lettered
		case '。', '！', '？':
			// no space follows a stop in Chinese or Japanese
			if trip, ok := c.end(lim); ok {
				return trip, true
			}
			continue
		default:
			c.stop = false
			c.lettered = c.lettered || unicode.IsLetter(r)
		}
		if len(c.cur) >= maxUnitBytes {
			// A unit this long has had no break. A loop with none in it
			// ("mcp mcp mcp …") never makes the window's worth of units, so
			// the block is read on its own.
			if words, ok := repeatedWords(string(c.cur)); ok {
				if c.runOn++; c.runOn >= runOnBlocks {
					return ReplyTrip{RunOn: true, Units: quoteAll(words), Window: c.runOn * maxUnitBytes, Distinct: len(words), UnitsRead: c.seen}, true
				}
			} else {
				c.runOn = 0
			}
			if trip, ok := c.end(lim); ok {
				return trip, true
			}
		}
	}
	return ReplyTrip{}, false
}

// end takes the unit written so far and says whether the window has become a
// loop with it.
func (c *channel) end(lim ReplyLimits) (ReplyTrip, bool) {
	raw := string(c.cur)
	if len(c.cur) < maxUnitBytes {
		// A break. A long unit before it that still is a few words keeps the
		// unbroken count; anything else is progress and ends it.
		if len(raw) < runOnLongLine {
			c.runOn = 0
		} else if _, ok := repeatedWords(raw); !ok {
			c.runOn = 0
		}
	}
	c.cur = c.cur[:0]
	c.stop, c.lettered = false, false
	key, ok := unitKey(raw)
	if !ok {
		return ReplyTrip{}, false
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	sum := h.Sum64()
	if c.counts == nil {
		c.counts = make(map[uint64]*unitStat)
		c.ring = make([]uint64, lim.Window)
	}
	slot := c.seen % len(c.ring)
	if c.seen >= len(c.ring) {
		old := c.ring[slot]
		if st := c.counts[old]; st != nil {
			if st.n--; st.n == 0 {
				delete(c.counts, old)
			}
		}
	}
	c.ring[slot] = sum
	st := c.counts[sum]
	if st == nil {
		st = &unitStat{text: quote(raw), at: c.seen}
		c.counts[sum] = st
	}
	st.n++
	c.seen++
	if c.seen < len(c.ring) || len(c.counts) > lim.Distinct {
		return ReplyTrip{}, false
	}
	all := make([]*unitStat, 0, len(c.counts))
	for _, st := range c.counts {
		all = append(all, st)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].at < all[j].at
	})
	trip := ReplyTrip{Window: len(c.ring), Distinct: len(all), UnitsRead: c.seen}
	for _, st := range all {
		trip.Units = append(trip.Units, st.text)
	}
	return trip, true
}

// unitKey is what tells a unit apart from others, and whether it counts: its
// words without case or its closing stop. A unit with no word in it (a brace, a
// rule, box drawing, a row of numbers or hex) is no sentence and is left out,
// which is what keeps code, tables and logs from reading as a loop.
func unitKey(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimRightFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(".!?。！？,，;；:：…", r)
	})
	if !hasWord(s) {
		return "", false
	}
	s = strings.ToLower(s)
	if len(s) > keyBytes {
		s = s[:keyBytes]
	}
	return s, true
}

// hasWord says whether s has a word in it: a run of letters, underscores
// included, with no digit among them (Now, Writing, 好), not 0x00, v2 or a hash.
func hasWord(s string) bool {
	letters, digits := false, false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			letters = letters || unicode.IsLetter(r)
			digits = digits || unicode.IsDigit(r)
			continue
		}
		if letters && !digits {
			return true
		}
		letters, digits = false, false
	}
	return letters && !digits
}

// repeatedWords says whether s, written with no break, is the same few words
// over and over ("mcp mcp mcp …", or "mcpmcpmcp…"), and which. A long line of
// prose, code, JSON, base64 or a table row has many different words (its
// names, numbers and cells) or just a few long ones.
func repeatedWords(s string) ([]string, bool) {
	if !hasWord(s) {
		return nil, false
	}
	// A single character repeated is no loop of words ("AAAA…" is what zeros
	// are in base64, a rule of dashes or a bar of equals signs is a drawing).
	if periodic(s, 1) {
		return nil, false
	}
	// One piece repeated back to back. The first period that holds is the
	// shortest one, so the piece is never itself a repetition of a smaller one.
	for p := 2; p <= runOnMaxPeriod && 4*p <= len(s); p++ {
		if !periodic(s, p) {
			continue
		}
		start := 0
		for start < p && !utf8.RuneStart(s[start]) {
			start++
		}
		// a few rounds of it, ending on a character's end
		end := min(start+3*p, len(s))
		for end < len(s) && !utf8.RuneStart(s[end]) {
			end++
		}
		piece := strings.TrimSpace(s[start:end])
		if !hasWord(piece) {
			return nil, false
		}
		return []string{piece}, true
	}
	counts := map[string]int{}
	var order []string
	words := 0
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		words++
		if counts[w]++; counts[w] == 1 {
			order = append(order, w)
		}
		if len(counts) > runOnDistinct {
			return nil, false
		}
	}
	if words*32 < len(s) {
		return nil, false // a few long words: not read as a loop
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	return order, true
}

// periodic says whether s is its first p bytes repeated to the end.
func periodic(s string, p int) bool {
	for i := p; i < len(s); i++ {
		if s[i] != s[i-p] {
			return false
		}
	}
	return true
}

// quote cuts a unit short to be quoted.
func quote(raw string) string {
	text := strings.TrimSpace(raw)
	if utf8.RuneCountInString(text) > quoteRunes {
		return string([]rune(text)[:quoteRunes]) + "…"
	}
	return text
}

func quoteAll(words []string) []string {
	out := make([]string, 0, len(words))
	for _, w := range words {
		out = append(out, quote(w))
	}
	return out
}

package privacymask

import (
	"sort"
	"strings"
)

// Running every rule over every text was most of masking's cost: an agent's
// request is mostly code, logs and tool output, and a text that merely has a
// marker somewhere (max_tokens has "token", sort_key "key") had a rule's
// pattern run over all of it. On a megabyte of such history the first pass
// took over a second (BenchmarkPlanOneMegabyteHistory), paid by every new
// conversation and by every one after a restart; magpie, one user's, pays it
// once and remembers (memo.go).
//
// No built-in rule but the private key's can match across a line: their
// classes leave the newline out. So a rule is run only over the lines that can
// hold a match of it: those with one of its markers (found with strings.Index
// over the whole text, not line by line), or, for a rule with none, those
// whose stats meet what it needs (a phone number's eleven digits in a row). A
// match found in a line is the match the whole text has there, as every check
// after the pattern (bound, ok, at, fix) reads the whole text.

// lineStats is a line of a text and what it has, counted in one pass.
type lineStats struct {
	start, end int
	// maxRun is its longest run of digits, digits all of them
	maxRun, digits int
	upper          int
	dots, colons   int
	dashes         int
}

func digitRun(n int) func(lineStats) bool   { return func(l lineStats) bool { return l.maxRun >= n } }
func digitCount(n int) func(lineStats) bool { return func(l lineStats) bool { return l.digits >= n } }
func ibanShape(l lineStats) bool            { return l.upper >= 2 && l.digits >= 2 }
func macShape(l lineStats) bool             { return l.colons >= 5 || l.dashes >= 5 }
func ipv4Shape(l lineStats) bool            { return l.dots >= 3 && l.digits >= 4 }
func ipv6Shape(l lineStats) bool            { return l.colons >= 2 }

// textLines splits s at its newlines, each line with its stats.
func textLines(s string) []lineStats {
	var out []lineStats
	cur := lineStats{}
	run := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\n':
			cur.end = i
			out = append(out, cur)
			cur, run = lineStats{start: i + 1}, 0
			continue
		case c >= '0' && c <= '9':
			cur.digits++
			run++
			cur.maxRun = max(cur.maxRun, run)
			continue
		case c >= 'A' && c <= 'Z':
			cur.upper++
		case c == '.':
			cur.dots++
		case c == ':':
			cur.colons++
		case c == '-':
			cur.dashes++
		}
		run = 0
	}
	cur.end = len(s)
	return append(out, cur)
}

// scanned is how a rule is run over s: lower is s with ASCII in lower case and
// lines its lines, each made once for all the rules, when one needs it.
type scanned struct {
	s     string
	lower string
	lines []lineStats
}

func (x *scanned) lowered() string {
	if x.lower == "" {
		x.lower = lowerASCII(x.s)
	}
	return x.lower
}

func (x *scanned) split() []lineStats {
	if x.lines == nil {
		x.lines = textLines(x.s)
	}
	return x.lines
}

// scan are r's matches in x.s, as FindAllStringSubmatchIndex has them.
func (r rule) scan(x *scanned) [][]int {
	if r.ahead > 0 {
		return r.matches(x.s, x.lowered()) // windows around its fold markers already
	}
	if r.multiline || r.yours {
		// a private key spans lines, and nothing is known of the user's own
		// patterns
		return r.findAll(x.s)
	}
	lines := x.split()
	if len(lines) == 1 {
		if r.needs != nil && !r.needs(lines[0]) {
			return nil
		}
		return r.findAll(x.s)
	}
	var cand []int
	switch {
	case len(r.markers) > 0 || len(r.fold) > 0:
		cand = markerLines(x, r.markers, r.fold)
	default:
		for i := range lines {
			if r.needs == nil || r.needs(lines[i]) {
				cand = append(cand, i)
			}
		}
	}
	var out [][]int
	for _, i := range cand {
		l := lines[i]
		if l.end-l.start < 3 || r.needs != nil && !r.needs(l) {
			continue
		}
		for _, m := range r.findAll(x.s[l.start:l.end]) {
			for k := range m {
				if m[k] >= 0 {
					m[k] += l.start
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// markerLines are the lines of x that have one of markers, or of fold in
// lower case, in order and each once.
func markerLines(x *scanned, markers, fold []string) []int {
	lines := x.split()
	seen := make(map[int]bool)
	var out []int
	find := func(text, m string) {
		for i := 0; ; {
			j := strings.Index(text[i:], m)
			if j < 0 {
				return
			}
			at := i + j
			n := sort.Search(len(lines), func(k int) bool { return lines[k].end >= at })
			if n < len(lines) && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
			// the rest of this line is in already
			i = max(at+len(m), lines[min(n, len(lines)-1)].end)
			if i >= len(text) {
				return
			}
		}
	}
	for _, m := range markers {
		find(x.s, m)
	}
	if len(fold) > 0 {
		lower := x.lowered()
		for _, m := range fold {
			find(lower, m)
		}
	}
	sort.Ints(out)
	return out
}

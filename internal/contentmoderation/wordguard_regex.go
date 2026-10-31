package contentmoderation

import (
	"fmt"
	"math"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// Go's regexp is RE2: matching time is linear in the text, so no pattern can
// backtrack catastrophically. Linear is not small, though. A pattern of a few
// hundred characters can compile to a program of millions of instructions
// (`(?:a{1000}){100}`, `\pL{1000}`) and the engine then pays that program's
// size for every byte it reads: on the machine this was measured on,
// `(?:a?){1000}a{1000}` takes 1.2 s to read 128 KB. Two bounds keep that cost
// from being set by whoever writes a pattern or sends a request:
//
//   - at save time, a pattern's compiled program (MaxWordRegexProgram) and the
//     sum over a policy (MaxWordRegexProgramTotal) are limited, and the size is
//     worked out from the parsed pattern before anything is expanded;
//   - at request time, the work of the regular expressions of one request or
//     one reply (program size times bytes read, see regexBudget) is limited.

// regexEstimateLimit is the estimate (see estimateRegexProgram) beyond which a
// pattern is refused without compiling it. The estimate is close to the real
// program size, so the slack only has to absorb its error; a pattern under it is
// then compiled and held to MaxWordRegexProgram exactly.
const regexEstimateLimit = 4 * MaxWordRegexProgram

// compiledWordRegex is a regular expression accepted for matching, with what
// is known about it from its syntax tree.
type compiledWordRegex struct {
	re   *regexp.Regexp
	rule int
	// program is the size of the compiled program: the unit of the work budget.
	program int
	// maxLen is the most characters a match can have, or -1 when a match can be
	// of any length (`\d+`).
	maxLen int
	// first is the set of characters a match can begin with.
	first runeSet
}

// compileWordRegex compiles a rule's expression the way matching will use it:
// case-insensitive, leftmost-longest, run against normalized text. The (?i) is
// a prefix rather than a wrapping group so that a pattern cannot close the
// group itself and smuggle in an alternative. It returns the problem, as a code
// and a message, when the expression cannot be used.
func compileWordRegex(pattern string) (compiledWordRegex, string, string) {
	const flags = "(?i)"
	parsed, err := syntax.Parse(flags+pattern, syntax.Perl)
	if err != nil {
		return compiledWordRegex{}, "invalid_regex", cleanRegexError(err)
	}
	if minRegexLen(parsed) == 0 {
		return compiledWordRegex{}, "matches_empty", "the expression can match an empty string, which would match every request"
	}
	// Judge the size before expanding anything: simplifying a repetition writes
	// out every copy of it, and nested repetitions multiply.
	if estimate := estimateRegexProgram(parsed); estimate > regexEstimateLimit {
		return compiledWordRegex{}, "regex_too_complex", regexTooComplex(estimate)
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return compiledWordRegex{}, "invalid_regex", cleanRegexError(err)
	}
	if len(prog.Inst) > MaxWordRegexProgram {
		return compiledWordRegex{}, "regex_too_complex", regexTooComplex(len(prog.Inst))
	}
	compiled, err := regexp.Compile(flags + pattern)
	if err != nil {
		return compiledWordRegex{}, "invalid_regex", cleanRegexError(err)
	}
	compiled.Longest()
	return compiledWordRegex{
		re:      compiled,
		program: len(prog.Inst),
		maxLen:  maxRegexLen(parsed),
		first:   regexFirstRunes(parsed),
	}, "", ""
}

// holdFrom is where, in text that more text may follow, a match of the
// expression could still begin without being visible yet, or -1.
//
// RE2 cannot say whether the end of a text is the start of a match, but an
// expression whose matches are at most maxLen characters long makes it a matter
// of counting: a match that begins with fewer than maxLen characters left could
// run past the end of the text, so every place in the last maxLen-1 characters
// where a match can begin is undecided. (A match that does not reach the end is
// already found, and one that does is held back by the caller.) Without this a
// number split across stream events, \d{11} read one digit at a time, would be
// let through ten digits at a time before the eleventh shows it was one.
//
// An expression whose matches have no longest length (\d+, \d{11,}) cannot be
// held back this way, and the caller holds back only a match it can see. That is
// a limit of streaming, not of the expression: it is shown beside such
// expressions in the panel.
func (c compiledWordRegex) holdFrom(text string) int {
	if c.maxLen < 0 {
		return -1
	}
	start := len(text)
	for left := c.maxLen - 1; left > 0 && start > 0; left-- {
		_, size := utf8.DecodeLastRuneInString(text[:start])
		start -= size
	}
	for p := start; p < len(text); {
		r, size := utf8.DecodeRuneInString(text[p:])
		if c.first.contains(r) {
			return p
		}
		p += size
	}
	return -1
}

// WordRegexIsBounded reports whether a usable expression has a longest match,
// which is what lets the reply filter hold back the start of a match across
// stream events (see compiledWordRegex.holdFrom). An expression that cannot be
// used as a rule (it does not parse, or can match nothing) is not bounded.
func WordRegexIsBounded(pattern string) bool {
	parsed, err := syntax.Parse("(?i)"+pattern, syntax.Perl)
	return err == nil && minRegexLen(parsed) > 0 && maxRegexLen(parsed) >= 0
}

func regexTooComplex(size int) string {
	about := fmt.Sprintf("about %d", size)
	if size > regexEstimateLimit {
		about = fmt.Sprintf("more than %d", regexEstimateLimit)
	}
	return fmt.Sprintf("the expression is too complex: it compiles to %s instructions and at most %d are allowed (counted repetitions such as {1000}, alone or nested, are the usual cause)",
		about, MaxWordRegexProgram)
}

// cleanRegexError drops the prefix the standard library adds and the (?i)
// this package puts in front of the operator's pattern.
func cleanRegexError(err error) string {
	message := strings.TrimPrefix(err.Error(), "error parsing regexp: ")
	return strings.ReplaceAll(message, "(?i)", "")
}

// minRegexLen is the fewest characters a match of re can have. Zero-width
// assertions (anchors, word boundaries) count as nothing, so an expression made
// only of them, or that can skip everything it names, reports zero.
func minRegexLen(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpCapture, syntax.OpPlus:
		return minRegexLen(re.Sub[0])
	case syntax.OpRepeat:
		return mulSaturating(re.Min, minRegexLen(re.Sub[0]))
	case syntax.OpConcat:
		total := 0
		for _, sub := range re.Sub {
			total = addSaturating(total, minRegexLen(sub))
		}
		return total
	case syntax.OpAlternate:
		shortest := -1
		for _, sub := range re.Sub {
			if n := minRegexLen(sub); shortest < 0 || n < shortest {
				shortest = n
			}
		}
		return max(shortest, 0)
	case syntax.OpNoMatch:
		// Matches nothing at all, which is harmless; it is not "empty".
		return 1
	default:
		// Empty match, anchors, word boundaries, star and quest.
		return 0
	}
}

// estimateRegexProgram is the number of instructions re compiles to, worked
// out from the tree as the parser left it. It follows what Simplify and Compile
// do closely enough to tell a small program from a huge one: a counted
// repetition is written out once per count, and the sizes of nested ones
// multiply. The arithmetic saturates, so no pattern can overflow it.
func estimateRegexProgram(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCapture:
		return addSaturating(estimateRegexProgram(re.Sub[0]), 2)
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest:
		return addSaturating(estimateRegexProgram(re.Sub[0]), 1)
	case syntax.OpConcat:
		total := 0
		for _, sub := range re.Sub {
			total = addSaturating(total, estimateRegexProgram(sub))
		}
		return total
	case syntax.OpAlternate:
		total := len(re.Sub) - 1
		for _, sub := range re.Sub {
			total = addSaturating(total, estimateRegexProgram(sub))
		}
		return total
	case syntax.OpRepeat:
		size := estimateRegexProgram(re.Sub[0])
		switch {
		case re.Max < 0: // x{n,}: n copies, the last one repeated
			return addSaturating(mulSaturating(size, max(re.Min, 1)), 1)
		case re.Max == 0:
			return 1
		default: // x{n,m}: m copies, the last m-n of them optional
			return addSaturating(mulSaturating(size, re.Max), re.Max-re.Min)
		}
	default:
		// Character classes, any character, assertions, the empty match: one
		// instruction each.
		return 1
	}
}

// maxRegexLen is the most characters a match of re can have, or -1 when there
// is no bound.
func maxRegexLen(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpCapture, syntax.OpQuest:
		return maxRegexLen(re.Sub[0])
	case syntax.OpStar, syntax.OpPlus:
		if maxRegexLen(re.Sub[0]) == 0 {
			return 0
		}
		return -1
	case syntax.OpRepeat:
		sub := maxRegexLen(re.Sub[0])
		switch {
		case sub == 0:
			return 0
		case sub < 0 || re.Max < 0:
			return -1
		}
		return mulSaturating(sub, re.Max)
	case syntax.OpConcat:
		total := 0
		for _, sub := range re.Sub {
			n := maxRegexLen(sub)
			if n < 0 {
				return -1
			}
			total = addSaturating(total, n)
		}
		return total
	case syntax.OpAlternate:
		longest := 0
		for _, sub := range re.Sub {
			n := maxRegexLen(sub)
			if n < 0 {
				return -1
			}
			longest = max(longest, n)
		}
		return longest
	default:
		// Assertions, the empty match, and a pattern that matches nothing.
		return 0
	}
}

// runeSet is a set of characters as sorted, non-overlapping ranges. The zero
// value is the empty set.
type runeSet struct {
	// ranges holds lo, hi pairs.
	ranges []rune
	// any says every character is in the set.
	any bool
}

func (s runeSet) contains(r rune) bool {
	if s.any {
		return true
	}
	// The first range that ends at or after r.
	i := sort.Search(len(s.ranges)/2, func(i int) bool { return s.ranges[2*i+1] >= r })
	return i < len(s.ranges)/2 && s.ranges[2*i] <= r
}

func (s runeSet) union(other runeSet) runeSet {
	if s.any || other.any {
		return runeSet{any: true}
	}
	if len(other.ranges) == 0 {
		return s
	}
	if len(s.ranges) == 0 {
		return other
	}
	pairs := make([][2]rune, 0, (len(s.ranges)+len(other.ranges))/2)
	for _, ranges := range [][]rune{s.ranges, other.ranges} {
		for i := 0; i < len(ranges); i += 2 {
			pairs = append(pairs, [2]rune{ranges[i], ranges[i+1]})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	merged := runeSet{ranges: make([]rune, 0, 2*len(pairs))}
	lo, hi := pairs[0][0], pairs[0][1]
	for _, pair := range pairs[1:] {
		if pair[0] <= hi+1 {
			hi = max(hi, pair[1])
			continue
		}
		merged.ranges = append(merged.ranges, lo, hi)
		lo, hi = pair[0], pair[1]
	}
	merged.ranges = append(merged.ranges, lo, hi)
	return merged
}

// regexFirstRunes is the set of characters a non-empty match of re can begin
// with: a stream that ends in a character outside it cannot end inside a match
// of re that has not finished.
func regexFirstRunes(re *syntax.Regexp) runeSet {
	first, _ := firstRunes(re)
	return first
}

// firstRunes returns the characters a match of re can begin with and whether re
// can match without consuming any (the next part of a concatenation then also
// contributes).
func firstRunes(re *syntax.Regexp) (runeSet, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return runeSet{}, true
		}
		first := runeSet{ranges: []rune{re.Rune[0], re.Rune[0]}}
		if re.Flags&syntax.FoldCase != 0 {
			for variant := unicode.SimpleFold(re.Rune[0]); variant != re.Rune[0]; variant = unicode.SimpleFold(variant) {
				first = first.union(runeSet{ranges: []rune{variant, variant}})
			}
		}
		return first, false
	case syntax.OpCharClass:
		return runeSet{ranges: append([]rune(nil), re.Rune...)}, false
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return runeSet{any: true}, false
	case syntax.OpNoMatch:
		return runeSet{}, false
	case syntax.OpCapture:
		return firstRunes(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		first, _ := firstRunes(re.Sub[0])
		return first, true
	case syntax.OpPlus:
		return firstRunes(re.Sub[0])
	case syntax.OpRepeat:
		first, nullable := firstRunes(re.Sub[0])
		return first, nullable || re.Min == 0
	case syntax.OpConcat:
		var first runeSet
		for _, sub := range re.Sub {
			part, nullable := firstRunes(sub)
			first = first.union(part)
			if !nullable {
				return first, false
			}
		}
		return first, true
	case syntax.OpAlternate:
		var first runeSet
		nullable := false
		for _, sub := range re.Sub {
			part, subNullable := firstRunes(sub)
			first = first.union(part)
			nullable = nullable || subNullable
		}
		return first, nullable
	default:
		// The empty match and the assertions consume nothing.
		return runeSet{}, true
	}
}

func addSaturating(a, b int) int {
	if a > math.MaxInt32 || b > math.MaxInt32 || a+b > math.MaxInt32 {
		return math.MaxInt32
	}
	return a + b
}

func mulSaturating(a, b int) int {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > math.MaxInt32/b {
		return math.MaxInt32
	}
	return a * b
}

// regexBudget bounds the work the regular expressions of one request, or of one
// reply, may do. The unit is a step: the size of an expression's program times
// the number of bytes it is run over, which is what the engine's worst case
// costs (about 4 ns a step on the machine this was measured on, whatever the
// expression, so the budget is about half a second of one core at most).
// Keywords are not charged: the trie reads each byte once.
//
// A scan that does not fit is not made: the expression sees none of that text,
// the request goes on, and the request log says so (see WordGuardSnapshot).
// Failing open is the cost of never stalling a request; the number is large
// enough that an ordinary expression (a few dozen instructions) reads several
// megabytes of text within it.
type regexBudget struct {
	left     atomic.Int64
	exceeded atomic.Bool
}

// maxRegexWork is the budget of one request or reply, in steps.
const maxRegexWork = 128 << 20

func newRegexBudget() *regexBudget {
	budget := &regexBudget{}
	budget.left.Store(maxRegexWork)
	return budget
}

// take spends steps and reports whether they were available. A nil budget has
// no limit.
func (b *regexBudget) take(steps int64) bool {
	if b == nil {
		return true
	}
	if b.left.Add(-steps) >= 0 {
		return true
	}
	b.left.Add(steps) // not spent: smaller scans may still fit
	b.exceeded.Store(true)
	return false
}

// wasExceeded says whether some scan was skipped for want of budget.
func (b *regexBudget) wasExceeded() bool {
	return b != nil && b.exceeded.Load()
}

package privacymask

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Options says what to mask. It is magpie's redact.Options without the local
// model, the made-up keys and the route (see doc.go).
type Options struct {
	Secrets  bool     // API keys, private keys, tokens, passwords
	Personal bool     // emails, phone numbers, ID and bank card numbers
	Words    []string // the tenant's or the user's own: names, codenames, hosts
	// WordsAnyCase matches Words in any case: Nightjar masks NIGHTJAR and
	// nightjar too. Off, a word is matched as written.
	WordsAnyCase bool
	// WordsWhole matches Words only as whole words: ABC masks "ABC IS" and
	// 'ABC' but not XABCD, and 123456 masks no part of 91234567, 0.123456 or
	// 123456.78 (wholeWord). Off, a word is found anywhere, inside a longer
	// one too.
	WordsWhole bool
	// Rules are the user's own. Unlike magpie's they apply with the secrets
	// off too: a tenant's rules apply to its portal users whatever they
	// switch, and a rule someone wrote is meant to mask.
	Rules []Rule
	// Kinds turns each of the Categories of personal data on or off by its id,
	// where the user chose; one not in it is as its Category.On says. None of
	// them is masked while Personal is off.
	Kinds map[string]bool
	// Actions is what is done with a value of a kind other than masking it,
	// where the user chose: Warn sends it as written, Block holds the request
	// back. A kind not in it is masked.
	Actions map[string]string
	// Allow are values the user lets go as written, whichever finder finds
	// them: each a whole value, in any case.
	Allow []string
	// UserAllow is Allow written by a portal user, and Locked the categories
	// ("secret", "personal") the tenant turned on for everyone: a portal user's
	// own list cannot let a value of a locked category go as written.
	UserAllow []string
	Locked    map[string]bool
}

// Any says o masks anything at all.
func (o Options) Any() bool {
	return o.Secrets || o.Personal || len(o.Words) > 0 || len(o.Rules) > 0
}

// covers says o masks what a rule of category cat finds: "" are the secrets,
// catPersonal the personal data always masked with it, any other one of the
// Categories.
func (o Options) covers(cat string) bool {
	switch cat {
	case "":
		return o.Secrets
	case catPersonal:
		return o.Personal
	}
	if !o.Personal {
		return false
	}
	if on, ok := o.Kinds[cat]; ok {
		return on
	}
	return categoryOn[cat]
}

// kindCats are the categories each kind of personal data is found under
// (Options.covers): a user name is under both home folders and user@host. A
// kind not in it is a secret's, the user's rules' among them.
var kindCats = func() map[string][]string {
	m := map[string][]string{}
	for _, r := range rules {
		kinds := r.groups
		if len(kinds) == 0 {
			kinds = []string{r.kind}
		}
		for _, k := range kinds {
			if r.cat != "" && !slices.Contains(m[k], r.cat) {
				m[k] = append(m[k], r.cat)
			}
		}
	}
	return m
}()

// Personal says kind is personal data rather than a secret.
func Personal(kind string) bool {
	_, ok := kindCats[kind]
	return ok
}

// coversKind says o masks a known value of kind: a personal one when one of
// the categories it is found under is on, any other when o masks secrets.
func (o Options) coversKind(kind string) bool {
	cats, ok := kindCats[kind]
	if !ok {
		return o.Secrets
	}
	for _, c := range cats {
		if o.covers(c) {
			return true
		}
	}
	return false
}

// span is a value found in a text: text[start:end].
type span struct {
	start, end int
	kind       string
	by         string // which finder found it: one of the By constants
	// score and bare are magpie's (its local model's score, a bare JSON value
	// in text that is JSON); kept so the ported finders build spans as they did
	score float64
	bare  bool
}

// Which finder found a value: the built-in rules, the user's own rules, the
// user's words, or a value masked before (known.go). ByModel is never set
// here (there is no local model); the ported finders still name it.
const (
	ByRule  = "rule"
	ByYours = "yours"
	ByWord  = "word"
	ByKnown = "known"
	ByModel = "model"
)

// What is done with a value of a kind (Options.Actions) other than masking it.
const (
	Mask  = "mask"
	Warn  = "warn"  // sent as written, and recorded
	Block = "block" // the request is held back
)

// actionOf is what o does with a value of kind: Mask, Warn or Block.
func (o Options) actionOf(kind string) string {
	switch a := o.Actions[kind]; a {
	case Warn, Block:
		return a
	}
	return Mask
}

// allows says v, of kind, goes as written (Options.Allow, Options.UserAllow).
func (o Options) allows(v, kind string) bool {
	match := func(list []string) bool {
		for _, a := range list {
			if strings.EqualFold(strings.TrimSpace(a), v) {
				return true
			}
		}
		return false
	}
	if match(o.Allow) {
		return true
	}
	return !o.Locked[Class(kind)] && match(o.UserAllow)
}

// Class is the switch a kind is masked under: "secret" for keys, tokens,
// passwords and the user's own rules, "personal" for personal data, "word"
// for the user's words.
func Class(kind string) string {
	switch {
	case kind == "TERM":
		return "word"
	case Personal(kind):
		return "personal"
	}
	return "secret"
}

// Hint is as much of v as may be kept where v itself may not: its first
// character (the first few of a key's, which name its kind: sk-, ghp_) and how
// many characters it has.
func Hint(kind, v string) (head string, n int) {
	n = utf8.RuneCountInString(v)
	if n < 2 {
		return "", n
	}
	k := 1
	if !Personal(kind) && kind != "TERM" && n >= 16 {
		// a key's prefix, up to where its random part starts
		k = 0
		for _, c := range v {
			if k >= 4 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_", c) {
				break
			}
			k++
			if c == '-' || c == '_' {
				break
			}
		}
		k = max(k, 1)
	}
	r := []rune(v)
	return string(r[:k]), n
}

// boundedCache is a map that starts over when it is full. The patterns made
// for the tenants' own words and rules are kept in them: every tenant and
// portal user may have their own, so an unbounded map would grow for as long
// as the process runs (magpie, one user's, keeps them all).
type boundedCache struct {
	mu  sync.Mutex
	max int
	m   map[string]*regexp.Regexp
}

func (c *boundedCache) get(key string, build func() *regexp.Regexp) *regexp.Regexp {
	c.mu.Lock()
	re, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return re
	}
	re = build()
	c.mu.Lock()
	if c.m == nil || len(c.m) >= c.max {
		c.m = make(map[string]*regexp.Regexp)
	}
	c.m[key] = re
	c.mu.Unlock()
	return re
}

// anyCases are the patterns that find a word in any case.
var anyCases = boundedCache{max: 4096}

// anyCase finds w as written but in any case (Unicode's simple folding, so a
// word whose case changes its length in bytes is still found whole).
func anyCase(w string) *regexp.Regexp {
	return anyCases.get(w, func() *regexp.Regexp { return regexp.MustCompile(`(?i)` + regexp.QuoteMeta(w)) })
}

// wordRune says r is a letter, a digit or _ of a script that puts spaces
// between words. Chinese and Japanese don't, so a word of theirs is found
// inside their text, and a Latin word right beside a Chinese letter ("用ABC做")
// is still whole.
func wordRune(r rune) bool {
	if r == '_' {
		return true
	}
	if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return false
	}
	return !unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}

// wholeWord says s[a:b], a word the user asked to mask, is a word of its own
// in s: where its first or last letter is a wordRune, the letter beside it in
// s isn't one. A number is one with its decimals, so a digit at its edge also
// has no "." and digit beside it: 123456 is not whole in 0.123456 or
// 123456.78, while "v1." ends a sentence.
func wholeWord(s string, a, b int) bool {
	first, _ := utf8.DecodeRuneInString(s[a:b])
	last, _ := utf8.DecodeLastRuneInString(s[a:b])
	if wordRune(first) && a > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:a])
		if wordRune(r) {
			return false
		}
		if unicode.IsDigit(first) && r == '.' {
			if p, _ := utf8.DecodeLastRuneInString(s[:a-n]); unicode.IsDigit(p) {
				return false
			}
		}
	}
	if wordRune(last) && b < len(s) {
		r, n := utf8.DecodeRuneInString(s[b:])
		if wordRune(r) {
			return false
		}
		if unicode.IsDigit(last) && r == '.' {
			if f, _ := utf8.DecodeRuneInString(s[b+n:]); unicode.IsDigit(f) {
				return false
			}
		}
	}
	return true
}

// wordSpans are where s has one of o's words (TERM), a date as that date
// however it is written (dateword.go).
func wordSpans(s string, o Options) []span {
	var found []span
	for _, w := range o.Words {
		if w = strings.TrimSpace(w); len(w) < 2 {
			continue
		}
		if spans, ok := dateSpans(s, w); ok {
			found = append(found, spans...)
			continue
		}
		// next finds w at or after i; one inside a longer word, where the user
		// asked for whole words, is passed over a letter at a time, so
		// "XABC ABC" still finds the second
		next := func(i int) (int, int) {
			if o.WordsAnyCase {
				if m := anyCase(w).FindStringIndex(s[i:]); m != nil {
					return i + m[0], i + m[1]
				}
				return -1, -1
			}
			if j := strings.Index(s[i:], w); j >= 0 {
				return i + j, i + j + len(w)
			}
			return -1, -1
		}
		for i := 0; i <= len(s); {
			a, b := next(i)
			if a < 0 {
				break
			}
			if o.WordsWhole && !wholeWord(s, a, b) {
				_, n := utf8.DecodeRuneInString(s[a:])
				i = a + max(n, 1)
				continue
			}
			found = append(found, span{a, b, "TERM", ByWord, 0, false})
			i = max(b, a+1)
		}
	}
	return found
}

// ruleSpans are what the rules, the built-in ones and the user's own, find in
// s.
func ruleSpans(s string, o Options) []span {
	return memoRules(s, o, func() []span {
		if len(s) >= ruleMemoMin {
			ruleRuns.Add(1)
		}
		var found []span
		all := rules
		if len(o.Rules) > 0 {
			// the user's first: where one of theirs and one of the built-in ones
			// find the same value, it goes by the name they gave it
			all = append(customRules(o.Rules), rules...)
		}
		x := &scanned{s: s}
		// split are matches a rule took apart into values of their own
		// (rkato@box17 as a user and a host): what another rule found from the
		// same place in them, an email, goes
		var split []span
		for _, r := range all {
			if !r.yours && !o.covers(r.cat) {
				continue
			}
			if len(r.markers) > 0 && !containsAny(s, r.markers) {
				continue
			}
			if len(r.fold) > 0 && !containsAny(x.lowered(), r.fold) {
				continue
			}
			for _, m := range r.scan(x) {
				if len(r.groups) > 0 {
					took := false
					for g, kind := range r.groups {
						if a, b := m[2*g+2], m[2*g+3]; a >= 0 && r.takes(s, a, b, r.groupOK[g]) {
							found = append(found, span{a, b, kind, r.by(), 0, false})
							took = true
						}
					}
					if took {
						split = append(split, span{m[2], m[1], "", "", 0, false})
					}
					continue
				}
				a, b := m[0], m[1]
				for g := 1; 2*g+1 < len(m); g++ {
					if m[2*g] >= 0 {
						a, b = m[2*g], m[2*g+1]
						break
					}
					if !r.anyGroup {
						break
					}
				}
				ok := r.ok
				if r.fix != nil {
					var good bool
					if a, b, good = r.fix(s, a, b); !good {
						continue
					}
					ok = nil
				}
				if r.takes(s, a, b, ok) {
					found = append(found, span{a, b, r.kind, r.by(), 0, false})
				}
			}
		}
		for _, sp := range split {
			found = slices.DeleteFunc(found, func(f span) bool {
				return f.kind == "EMAIL" && f.start == sp.start && f.end <= sp.end
			})
		}
		return found
	})
}

// noNumbers is found without what is a piece of a decimal number of s: digits
// right after the point of 5.13800138000, or right before the point of
// 13800138000.5. A number is judged whole, and the fraction of a height, a
// price or a coordinate is not a phone or a card number because its digits
// happen to check. Nor is a number that stands bare as a JSON value:
// "timestamp": 17929036837 in a session file is a count or a time, and a
// phone, a card or an ID number in JSON is a string. Only personal data's
// kinds, whoever found them (a rule, a value known before): a secret is masked
// wherever it is, and what the user chose (their words, their rules) stays
// theirs.
func noNumbers(s string, found []span) []span {
	return slices.DeleteFunc(found, func(f span) bool {
		return Personal(f.kind) && f.by != ByWord && f.by != ByYours && (inDecimal(s, f.start, f.end) || jsonNumber(s, f.start, f.end))
	})
}

// jsonNumber says s[a:b] is all of a bare JSON number, by where it sits:
// digits only, an object's value ("key": …) or an element of an array of
// numbers ([…, …]), and nothing after it but the , } or ] that ends a value.
// "phone": "13800138000" is a string, and phone:13800138000 and "call
// 13800138000, thanks" are text, so each is still masked.
func jsonNumber(s string, a, b int) bool {
	if a >= b || strings.Trim(s[a:b], digits) != "" {
		return false
	}
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	k := b
	for k < len(s) && space(s[k]) {
		k++
	}
	if k < len(s) && strings.IndexByte(",}]", s[k]) < 0 {
		return false
	}
	i := a - 1
	for i >= 0 && space(s[i]) {
		i--
	}
	if i < 0 {
		return false
	}
	switch s[i] {
	case ':':
		for i--; i >= 0 && space(s[i]); i-- {
		}
		return i >= 0 && s[i] == '"'
	case '[', ',':
		// back over the numbers before it to the array's [
		for ; i >= 0; i-- {
			if s[i] == '[' {
				return true
			}
			if strings.IndexByte(digits+",.-+eE", s[i]) < 0 && !space(s[i]) {
				return false
			}
		}
	}
	return false
}

// inDecimal says s[a:b] starts with a digit that is a decimal's fraction (a
// digit and a point before it) or ends with a digit that is its whole part (a
// point and a digit after it).
func inDecimal(s string, a, b int) bool {
	isDigit := func(i int) bool { return i >= 0 && i < len(s) && s[i] >= '0' && s[i] <= '9' }
	return isDigit(a) && a >= 2 && s[a-1] == '.' && isDigit(a-2) ||
		isDigit(b-1) && b+1 < len(s) && s[b] == '.' && isDigit(b+1)
}

// by is the finder r is.
func (r rule) by() string {
	if r.yours {
		return ByYours
	}
	return ByRule
}

// matches are r's matches in s, lower being lowerASCII(s), as
// FindAllStringSubmatchIndex has them: in all of s, or, for a rule with ahead
// set, only around its fold markers.
func (r rule) matches(s, lower string) [][]int {
	if r.ahead == 0 {
		return r.findAll(s)
	}
	var wins [][2]int
	for _, f := range r.fold {
		for i := 0; ; {
			j := strings.Index(lower[i:], f)
			if j < 0 {
				break
			}
			at := i + j
			i = at + len(f)
			// from the line's start, or from where a word starts no further back
			// than back: what the pattern takes for the text's start is where a
			// line or a word starts in s too. The line's start is looked for no
			// further back than that: a text of one long line, as a request body
			// is, isn't read back to its start for each marker.
			start := max(0, at-r.back)
			if nl := strings.LastIndexByte(s[start:at], '\n'); nl >= 0 {
				start += nl + 1
			} else if start > 0 {
				for start < at && (strings.IndexByte(alnum+"_.-", s[start-1]) >= 0 || s[start]&0xc0 == 0x80) {
					start++
				}
				if start == at && strings.IndexByte(alnum+"_", s[at-1]) >= 0 {
					continue // inside a longer word
				}
			}
			wins = append(wins, [2]int{start, min(len(s), i+r.ahead)})
		}
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i][0] < wins[j][0] })
	var out [][]int
	for k := 0; k < len(wins); k++ {
		w := wins[k]
		for k+1 < len(wins) && wins[k+1][0] <= w[1] {
			k++
			w[1] = max(w[1], wins[k][1])
		}
		for _, m := range r.findAll(s[w[0]:w[1]]) {
			for x := range m {
				if m[x] >= 0 {
					m[x] += w[0]
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// findAll are r's matches in s: each of them, or, for a rule that reads a
// field's value after its name (field), none whose value is the next field
// (fieldMatches).
func (r rule) findAll(s string) [][]int {
	if r.field {
		return fieldMatches(r.re, s)
	}
	return r.re.FindAllStringSubmatchIndex(s, -1)
}

// lowerASCII is s with its ASCII letters in lower case, as long as s, so a
// place in one is the same place in the other (a byte of a character outside
// ASCII is never an ASCII letter). It is s itself, not a copy, where s has no
// capital.
func lowerASCII(s string) string {
	i := 0
	for i < len(s) && !('A' <= s[i] && s[i] <= 'Z') {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// takes says s[a:b] is a value r masks: nothing it may not touch beside it,
// and its checks pass.
func (r rule) takes(s string, a, b int, ok func(string) bool) bool {
	if b <= a {
		return false
	}
	if r.bound != "" && (a > 0 && strings.IndexByte(r.bound, s[a-1]) >= 0 || b < len(s) && strings.IndexByte(r.bound, s[b]) >= 0) {
		return false
	}
	if ok != nil && !ok(s[a:b]) {
		return false
	}
	return r.at == nil || r.at(s, a, b)
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

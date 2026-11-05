package privacymask

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// placeholderBody is a placeholder without its braces, as a model may write
// one back: API_KEY_k3v9x2mq, phone_b5puzbfv, PHONE_B5PUZBFV.
var placeholderBody = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*_[A-Za-z2-7]{8}(?:[A-Za-z2-7]{8})?`)

// maxPlaceholder is the longest placeholder a Masker writes: {{, a kind (a
// user's rule may name one up to maxKind long), _, the longer tag, }}.
const maxPlaceholder = len("{{") + maxKind + len("_") + 16 + len("}}")

// Restorer puts the values of one request back into its reply. It restores
// the placeholders of the request and those its scope knows, and nothing
// else: a reply cannot have another sender's value put back by naming its
// placeholder. Safe for concurrent use.
type Restorer struct {
	table map[string]string
	scope *Scope
	kinds map[string]bool

	mu     sync.Mutex
	counts map[string]int
}

// Restorer returns what puts this request's values back, nil when it has
// none. The masker is done with: the restorer reads its table.
func (m *Masker) Restorer() *Restorer {
	if len(m.table) == 0 {
		return nil
	}
	kinds := make(map[string]bool, len(m.kinds))
	for k := range m.kinds {
		kinds[k] = true
	}
	m.scope.kinds(kinds)
	return &Restorer{table: m.table, scope: m.scope, kinds: kinds, counts: make(map[string]int)}
}

// lookup is the value of placeholder p for this reply.
func (r *Restorer) lookup(p string) (string, bool) {
	if v, ok := r.table[p]; ok {
		return v, true
	}
	return r.scope.value(p)
}

// Restore puts the values back in s. escaped is for text that is itself JSON
// (a tool call's arguments), where a value goes in escaped.
//
// A model doesn't always write a placeholder back as it was given: it writes
// {{ PHONE_b5puzbfv }} as a template has it, drops a brace, or the braces, or
// changes the case. Any of those is put back too when what is left names a
// placeholder of this reply's: its kind and its tag, which no other text has
// by chance.
func (r *Restorer) Restore(s string, escaped bool) string {
	if r == nil || !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range placeholderBody.FindAllStringIndex(s, -1) {
		a, z := m[0], m[1]
		if a < last || a > 0 && isWordByte(s[a-1]) || z < len(s) && isWordByte(s[z]) {
			continue
		}
		p := canonical(s[a:z])
		v, ok := r.lookup(p)
		if !ok {
			continue
		}
		a, z = braces(s, a, z)
		if a < last {
			continue
		}
		r.mu.Lock()
		r.counts[p]++
		r.mu.Unlock()
		b.WriteString(s[last:a])
		if escaped {
			v = jsonEscape(v)
		}
		b.WriteString(v)
		last = z
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// Restored is how many places the reply had a value put back, so far.
func (r *Restorer) Restored() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.counts {
		n += c
	}
	return n
}

// canonical is the placeholder written for body, whatever its case.
func canonical(body string) string {
	cut := strings.LastIndexByte(body, '_')
	return "{{" + strings.ToUpper(body[:cut]) + "_" + strings.ToLower(body[cut+1:]) + "}}"
}

func isWordByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// braces widens s[a:z], a placeholder's body, over the braces the model wrote
// around it, and the spaces inside them: both of {{ and }}, or one of them
// dropped ({{X} or {X}}), or just {{ or just }}. A single brace on one side
// alone is the text's own ({X as in code) and stays.
func braces(s string, a, z int) (int, int) {
	la, l := a, 0
	for la > 0 && (s[la-1] == ' ' || s[la-1] == '\t') {
		la--
	}
	for l < 2 && la > 0 && s[la-1] == '{' {
		la--
		l++
	}
	rz, r := z, 0
	for rz < len(s) && (s[rz] == ' ' || s[rz] == '\t') {
		rz++
	}
	for r < 2 && rz < len(s) && s[rz] == '}' {
		rz++
		r++
	}
	if l == 2 || l == 1 && r >= 1 {
		a = la
	}
	if r == 2 || r == 1 && l >= 1 {
		z = rz
	}
	return a, z
}

// PartialTail is how much of the end of s may be the start of a placeholder
// the next piece of a stream finishes, in any of the ways Restore takes one
// back: {{API_, {{ phone_b5, PHONE_b5puzbfv} — or, with no brace before it, a
// last word that starts as a placeholder of one of this reply's kinds does
// (PHO, PHONE_b5pu), or is one whose }} may still come. len(s) less that is
// where the text may go out up to.
func (r *Restorer) PartialTail(s string) int {
	if r == nil {
		return 0
	}
	if n := braceTail(s); n > 0 {
		return n
	}
	return r.wordTail(s)
}

// braceTail is a tail from {, or {{, on: then spaces, a placeholder's body or
// the start of one, then spaces and one } at most.
func braceTail(s string) int {
	i := strings.LastIndexByte(s, '{')
	if i < 0 {
		return 0
	}
	if i > 0 && s[i-1] == '{' {
		i--
	}
	t := s[i:]
	if len(t) >= maxPlaceholder+4 || strings.Contains(t, "}}") {
		return 0
	}
	body := strings.TrimLeft(t, "{")
	body = strings.TrimLeft(body, " \t")
	if body == "" {
		return len(t) // {, {{, {{␠
	}
	if c := body[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		return 0
	}
	j := 0
	for j < len(body) && isWordByte(body[j]) {
		j++
	}
	rest := strings.TrimLeft(body[j:], " \t")
	if rest != "" && rest != "}" {
		return 0
	}
	if j < len(body) && placeholderBody.FindString(body[:j]) != body[:j] {
		return 0 // a space or a brace after something that is no placeholder
	}
	return len(t)
}

// wordTail is the last word of s, where it may be a placeholder's body as a
// model writes one without braces: the start of one of this reply's kinds, up
// to its tag. A word of text that only starts like a kind is held for an event
// and then goes on, unchanged.
func (r *Restorer) wordTail(s string) int {
	i := len(s)
	for i > 0 && isWordByte(s[i-1]) {
		i--
	}
	w := s[i:]
	if w == "" || len(w) > maxKind+17 || !(w[0] >= 'A' && w[0] <= 'Z' || w[0] >= 'a' && w[0] <= 'z') {
		return 0
	}
	u := strings.ToUpper(w)
	for k := range r.kinds {
		k += "_"
		if strings.HasPrefix(k, u) {
			return len(w)
		}
		if strings.HasPrefix(u, k) && len(u)-len(k) <= 16 && strings.Trim(strings.ToLower(u[len(k):]), "abcdefghijklmnopqrstuvwxyz234567") == "" {
			return len(w)
		}
	}
	return 0
}

// jsonEscape is v as the text of a JSON string, without its quotes: what a
// value goes in as inside a tool call's arguments.
func jsonEscape(v string) string {
	if !strings.ContainsAny(v, "\"\\") && !hasControl(v) {
		return v
	}
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
				continue
			}
			if r == utf8.RuneError {
				b.WriteRune(r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hasControl(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 {
			return true
		}
	}
	return false
}

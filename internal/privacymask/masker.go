package privacymask

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"regexp"
	"sort"
	"strings"
	"time"
)

// placeholderRe is a placeholder as a Masker writes it: {{, a kind, _, and the
// tag (8 characters, 16 for the rare value whose 8 are another's in the same
// request).
var placeholderRe = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*_[a-z2-7]{8}(?:[a-z2-7]{8})?\}\}`)

// tagEncoding writes a placeholder's tag in lower case base32.
var tagEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Value is one value a request had found: what it is, how it was found, what
// was done with it and how many places had it. It never holds the value, nor
// its placeholder.
type Value struct {
	Kind   string `json:"kind"`
	By     string `json:"by"`
	Action string `json:"action,omitempty"` // "" masked, Warn, Block
	// Head is the value's first character, or a key's prefix (sk-, ghp_), and
	// Len its length in characters (Hint).
	Head string `json:"head,omitempty"`
	Len  int    `json:"len"`
	N    int    `json:"n"`

	placeholder string
}

// Edit replaces text[Start:End] with Text.
type Edit struct {
	Start, End int
	Text       string
}

// Masker masks the texts of one request. It is used by one goroutine; what it
// hands to the reply (Restorer) is read-only.
type Masker struct {
	key   []byte
	opts  Options
	scope *Scope
	route string

	// table is every placeholder of this request with its value, and kinds the
	// kinds among them
	table map[string]string
	kinds map[string]bool
	// values are the values found, in the order first found; byP indexes the
	// masked ones by placeholder, and warned the ones sent as written by value
	values []*Value
	byP    map[string]*Value
	warned map[string]*Value
	places int
	took   time.Duration
	// quiet says the texts being planned are a second copy (PlanQuiet)
	quiet bool
}

// NewMasker returns a masker for one request: key is the tenant's placeholder
// key, scope what the sender's requests masked before (nil keeps nothing
// across requests), route what the request is sent on (the model asked for).
func NewMasker(key []byte, opts Options, scope *Scope, route string) *Masker {
	return &Masker{key: key, opts: opts, scope: scope, route: route,
		table: make(map[string]string), kinds: make(map[string]bool),
		byP: make(map[string]*Value), warned: make(map[string]*Value)}
}

// placeholder is the placeholder of v of kind. The tag is the start of
// HMAC-SHA256(tenant key, kind, 0, value): the same value always gets the same
// placeholder in a tenant, so a conversation the client sends again turn after
// turn reaches the upstream byte for byte the same and its prompt cache keeps
// working, while another tenant, with another key, gets another placeholder
// for it. The upstream cannot try candidate values against a tag without the
// key.
func (m *Masker) placeholder(kind, v string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(v))
	sum := mac.Sum(nil)
	p := "{{" + kind + "_" + tagEncoding.EncodeToString(sum[:5]) + "}}"
	if known, taken := m.table[p]; taken && known != v {
		// two values of one request on the same 40-bit tag: the longer tag tells
		// them apart, and is as stable
		p = "{{" + kind + "_" + tagEncoding.EncodeToString(sum[:10]) + "}}"
	}
	return p
}

// Plan finds what to mask in the texts of one request and returns, for each,
// the edits that mask it. skip[i] leaves text i alone: it is not read, and a
// value in it is neither found nor masked (a block the upstream signed).
//
// The rules and the words run over every text first, so a value found
// anywhere in the request is known for all of it, then the values known to the
// scope; a text the scope sent before keeps the known values it went with
// (sentSpans).
func (m *Masker) Plan(texts []string, skip []bool) [][]Edit {
	start := time.Now()
	defer func() { m.took += time.Since(start) }()
	out := make([][]Edit, len(texts))
	if !m.opts.Any() {
		return out
	}
	found := make([][]span, len(texts))
	index := &knownIndex{scope: m.scope}
	for i, s := range texts {
		if skip != nil && skip[i] || len(s) < 3 {
			continue
		}
		f := wordSpans(s, m.opts)
		f = append(f, ruleSpans(s, m.opts)...)
		f = noNumbers(s, noIDs(s, f))
		found[i] = f
		for _, sp := range f {
			if sp.by != ByWord {
				index.addOwn(s[sp.start:sp.end], sp.kind)
			}
		}
	}
	for i, s := range texts {
		if skip != nil && skip[i] || len(s) < 3 {
			continue
		}
		known := m.scope.sentSpans(m.route, s, m.opts, func() []span { return index.knownSpans(s, m.opts) })
		f := append(found[i], noNumbers(s, noIDs(s, known))...)
		out[i] = m.edits(s, f)
		m.learnPlaceholders(s)
	}
	return out
}

// PlanQuiet is Plan for a second copy of a request whose first copy was
// planned already (the body the executors read beside the one they
// translate): its values get the same placeholders and are counted once, with
// the first copy.
func (m *Masker) PlanQuiet(texts []string, skip []bool) [][]Edit {
	m.quiet = true
	defer func() { m.quiet = false }()
	return m.Plan(texts, skip)
}

// edits decides each of found in s: the first to start, and of those the
// longest, wins where they overlap, and nothing inside a placeholder already
// there is touched. A value o allows goes as written, as does one of a kind o
// warns of (recorded); the rest become placeholders.
func (m *Masker) edits(s string, found []span) []Edit {
	if len(found) == 0 {
		return nil
	}
	if strings.Contains(s, "{{") {
		for _, p := range placeholderRe.FindAllStringIndex(s, -1) {
			kept := found[:0]
			for _, f := range found {
				if !(f.start < p[1] && f.end > p[0]) {
					kept = append(kept, f)
				}
			}
			found = kept
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	var edits []Edit
	next := 0
	for _, f := range found {
		if f.start < next || f.end <= f.start {
			continue
		}
		next = f.end
		v := s[f.start:f.end]
		if m.opts.allows(v) {
			continue
		}
		act := m.opts.actionOf(f.kind)
		if act == Warn {
			if !m.quiet {
				m.note(f, v, "", Warn)
			}
			continue
		}
		p := m.placeholder(f.kind, v)
		m.table[p] = v
		m.kinds[f.kind] = true
		edits = append(edits, Edit{Start: f.start, End: f.end, Text: p})
		if !m.quiet {
			m.note(f, v, p, act)
			m.places++
		}
	}
	return edits
}

// note counts one place of v.
func (m *Masker) note(f span, v, p, act string) {
	if act == Mask {
		act = ""
	}
	var have *Value
	if p != "" {
		have = m.byP[p]
	} else {
		have = m.warned[f.kind+"\x00"+v]
	}
	if have == nil {
		head, n := Hint(f.kind, v)
		have = &Value{Kind: f.kind, By: f.by, Action: act, Head: head, Len: n, placeholder: p}
		m.values = append(m.values, have)
		if p != "" {
			m.byP[p] = have
		} else {
			m.warned[f.kind+"\x00"+v] = have
		}
	}
	have.N++
}

// learnPlaceholders adds to the table the placeholders s holds already that
// the scope knows: text a client sends back as the upstream wrote it (a
// summary of an earlier turn) can name one, and the reply may name it again.
func (m *Masker) learnPlaceholders(s string) {
	if m.scope == nil || !strings.Contains(s, "{{") {
		return
	}
	for _, p := range placeholderRe.FindAllString(s, -1) {
		if _, ok := m.table[p]; ok {
			continue
		}
		if v, ok := m.scope.value(p); ok {
			m.table[p] = v
			m.kinds[kindOfPlaceholder(p)] = true
		}
	}
}

// kindOfPlaceholder is the kind in a placeholder: SECRET of {{SECRET_abcdefgh}}.
func kindOfPlaceholder(p string) string {
	if i := strings.LastIndexByte(p, '_'); i > 2 {
		return p[2:i]
	}
	return ""
}

// Values are the values the request had found, masked or not.
func (m *Masker) Values() []Value {
	out := make([]Value, len(m.values))
	for i, v := range m.values {
		out[i] = *v
	}
	return out
}

// Places is how many places had a value replaced.
func (m *Masker) Places() int { return m.places }

// Took is how long Plan took.
func (m *Masker) Took() time.Duration { return m.took }

// Blocked are the kinds of the values whose kind holds the request back.
func (m *Masker) Blocked() []Value {
	var out []Value
	for _, v := range m.values {
		if v.Action == Block {
			out = append(out, *v)
		}
	}
	return out
}

// Commit makes the values this request masked known to its scope, for the
// requests after it. A request that was not sent is not committed.
func (m *Masker) Commit() {
	if m.scope == nil || len(m.byP) == 0 {
		return
	}
	m.scope.mu.Lock()
	defer m.scope.mu.Unlock()
	for p, v := range m.byP {
		m.scope.add(p, m.table[p], v.Kind)
	}
}

// Apply makes the edits, in order and not overlapping, to text.
func Apply(text string, edits []Edit) string {
	if len(edits) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(text[last:e.Start])
		b.WriteString(e.Text)
		last = e.End
	}
	b.WriteString(text[last:])
	return b.String()
}

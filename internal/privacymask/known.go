package privacymask

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// A value masked once is masked again wherever it is, as it is, whatever is
// around it. The rules find a value by what is around it (DB_PASSWORD=, a
// bound an email may not touch), and the model answers about a placeholder in
// words of its own: "the password is {{SECRET_…}}". The client reads the value
// there (the reply is restored), and on the next turn that text comes back in
// the history, where no rule matches it. Matched as known, it goes as the same
// placeholder again. (magpie's known.go; here per scope, see doc.go.)
//
// Within one request every value a rule found anywhere in it is known for all
// of it (Masker.Plan), so a password found in DB_PASSWORD=… is also masked in a
// tool call that has it bare. Across requests a Scope remembers the values its
// requests masked, in memory only.

// minKnown is the shortest value matched as known. Shorter ones (a password of
// three letters in a URL) would turn up all over ordinary text; 8 is what the
// rule for a password in an assignment asks of its value.
const minKnown = 8

// Bounds of what a scope keeps, and of how many scopes are kept.
const (
	maxScopeValues = 1024
	// sentEntries bounds the texts a scope remembers having sent (sentSpans).
	sentEntries = 1 << 13
	// maxScopes bounds the scopes of the process; past it the one used longest
	// ago goes. A scope dropped loses nothing a request needs: the next one
	// brings its values again, and its texts are masked afresh.
	maxScopes = 4096
	// scopeIdle is how long a scope that nothing used is kept.
	scopeIdle = 24 * time.Hour
)

type knownValue struct{ v, p, kind string }

// Scope is what the requests of one sender have masked: a tenant's portal
// user, one of its API keys, or the tenant as a whole. A reply is only ever
// restored from its own scope, so another sender cannot have a placeholder of
// someone else's turned back into the value.
type Scope struct {
	mu sync.Mutex
	// values are the placeholders made, with their values and kinds
	values map[string]knownValue
	// byHead are the values matched as known, by their first minKnown bytes,
	// and bits has a bit for the first four bytes of each
	byHead map[string][]knownValue
	bits   [1 << 10]uint64
	// used is when each placeholder was last made or met again, by clock
	used  map[string]uint64
	clock uint64
	// sentCur and sentOld remember how each text was masked the first time
	// (sentSpans): two generations, the newer becoming the older when full
	sentCur, sentOld map[[32]byte][]span
	last             time.Time
}

func newScope(now time.Time) *Scope {
	return &Scope{values: make(map[string]knownValue), byHead: make(map[string][]knownValue), used: make(map[string]uint64), last: now}
}

// gram16 hashes four bytes of s at i into 16 bits.
func gram16(s string, i int) uint32 {
	g := uint32(s[i]) | uint32(s[i+1])<<8 | uint32(s[i+2])<<16 | uint32(s[i+3])<<24
	return (g * 2654435761) >> 16
}

// add remembers that p stands for v of kind; called with s.mu held.
func (s *Scope) add(p, v, kind string) {
	if _, ok := s.values[p]; !ok {
		if len(s.values) >= maxScopeValues {
			s.makeRoom()
		}
		s.values[p] = knownValue{v, p, kind}
		s.index(knownValue{v, p, kind})
	}
	s.clock++
	s.used[p] = s.clock
}

// index makes k matchable as known: not the user's words, which are matched
// anywhere already, nor a value too short to be told from ordinary text.
func (s *Scope) index(k knownValue) {
	if k.kind == "TERM" || len(k.v) < minKnown {
		return
	}
	head := k.v[:minKnown]
	for i, o := range s.byHead[head] {
		if o.v == k.v {
			// one value under two kinds: the same one wins whichever came first
			if k.p < o.p {
				s.byHead[head][i] = k
			}
			return
		}
	}
	s.byHead[head] = append(s.byHead[head], k)
	g := gram16(k.v, 0)
	s.bits[g/64] |= 1 << (g % 64)
}

// makeRoom lets the half of the values used longest ago go; called with s.mu
// held. A request that still has them brings them back.
func (s *Scope) makeRoom() {
	ps := make([]string, 0, len(s.values))
	for p := range s.values {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return s.used[ps[i]] < s.used[ps[j]] })
	for _, p := range ps[:len(ps)/2] {
		delete(s.values, p)
		delete(s.used, p)
	}
	s.byHead = make(map[string][]knownValue)
	s.bits = [1 << 10]uint64{}
	for _, k := range s.values {
		s.index(k)
	}
}

// value is the value p stands for in this scope.
func (s *Scope) value(p string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.values[p]
	return k.v, ok
}

// kinds adds the kinds of the scope's placeholders to into.
func (s *Scope) kinds(into map[string]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.values {
		into[k.kind] = true
	}
}

// knownIndex is what a request matches as known: its own values (found by a
// rule anywhere in it) and its scope's.
type knownIndex struct {
	scope *Scope
	// own are the request's, by head, and ownBits as Scope.bits
	own     map[string][]knownValue
	ownBits [1 << 10]uint64
}

func (x *knownIndex) addOwn(v, kind string) {
	if kind == "TERM" || len(v) < minKnown {
		return
	}
	if x.own == nil {
		x.own = make(map[string][]knownValue)
	}
	head := v[:minKnown]
	for _, o := range x.own[head] {
		if o.v == v {
			return
		}
	}
	x.own[head] = append(x.own[head], knownValue{v: v, kind: kind})
	g := gram16(v, 0)
	x.ownBits[g/64] |= 1 << (g % 64)
}

// knownSpans are where s has a known value o masks (coversKind). Where two
// start at the same place, the longer.
func (x *knownIndex) knownSpans(s string, o Options) []span {
	if !o.Secrets && !o.Personal || len(s) < minKnown {
		return nil
	}
	if x.scope != nil {
		x.scope.mu.Lock()
		defer x.scope.mu.Unlock()
	}
	var out []span
	for i := 0; i+minKnown <= len(s); i++ {
		g := gram16(s, i)
		inOwn := x.ownBits[g/64]&(1<<(g%64)) != 0
		inScope := x.scope != nil && x.scope.bits[g/64]&(1<<(g%64)) != 0
		if !inOwn && !inScope {
			continue
		}
		head := s[i : i+minKnown]
		var best *knownValue
		consider := func(list []knownValue) {
			for j := range list {
				k := &list[j]
				if !o.coversKind(k.kind) {
					continue
				}
				if best != nil && len(k.v) <= len(best.v) || !strings.HasPrefix(s[i:], k.v) || !standsAlone(k.kind, s, i, i+len(k.v)) {
					continue
				}
				best = k
			}
		}
		if inOwn {
			consider(x.own[head])
		}
		if inScope {
			consider(x.scope.byHead[head])
		}
		if best != nil {
			out = append(out, span{i, i + len(best.v), best.kind, ByKnown, 0, false})
			i += len(best.v) - 1
		}
	}
	return out
}

// standsAlone says s[a:b] is the known value itself, not a piece of a longer
// one: a number with no letter or digit beside it (as the rules take one:
// 9f13800138000abc is a hash's piece), an email that is not part of a longer
// address (though a full stop may end the sentence after it), a secret with no
// letter or digit against it (twelve hex digits a rule of the user's took are
// also in the middle of a hash).
func standsAlone(kind, s string, a, b int) bool {
	before, after := byte(0), byte(0)
	if a > 0 {
		before = s[a-1]
	}
	if b < len(s) {
		after = s[b]
	}
	is := func(c byte, set string) bool { return c != 0 && strings.IndexByte(set, c) >= 0 }
	switch kind {
	case "PHONE", "BANK_CARD", "ID_CARD", "IBAN", "SSN", "PASSPORT", "SERIAL":
		return !is(before, alnum) && !is(after, alnum)
	case "IP", "MAC":
		// not a piece of a longer address or a version: 1.2.3.45, 1.2.3.4.5
		if is(before, alnum+".:") || is(after, alnum+":") {
			return false
		}
		return after != '.' || b+1 >= len(s) || !is(s[b+1], digits)
	case "USERNAME":
		return nameAlone(s, a, b)
	case "EMAIL":
		if is(before, alnum+"._%+-") || is(after, alnum+"_%+") {
			return false
		}
		// a.b@c.com.cn is another address; a.b@c.com. ends a sentence
		return !is(after, ".-") || b+1 >= len(s) || !is(s[b+1], alnum)
	}
	return !(is(before, alnum) && is(s[a], alnum)) && !(is(after, alnum) && is(s[b-1], alnum))
}

// A text sent before goes again as it went: the known values found in it the
// first time it was masked for a route are the ones masked in it there from
// then on. A value that became known since is masked in new text only.
//
// A client sends its whole conversation on every turn, and an upstream caches
// it by its bytes up to a mark (Anthropic's cache_control, the prefix OpenAI
// and DeepSeek cache on their own). In magpie, a value found by a rule in a
// turn's tool output became known, and the next turn masked it again in the
// system prompt and every earlier message that had it as written: the prefix
// changed at its first block, and a 400K-token conversation was written to the
// cache whole instead of read (cache_read 0, cache_write 412,081 to 426,782 on
// four turns). Masking it there keeps nothing from the upstream: the text went
// to it as written.
//
// That holds only where it went, so what is remembered is kept by route (the
// model asked for): on another one the text is masked with what is known now.
// What is remembered is a hash of the route, the text and of what o covers,
// and the places masked in it; nothing of a value. A text no longer remembered
// (sentEntries, a scope dropped, the process started again) is masked with what
// is known then.

// sentSpans is find()'s spans for a text going upstream: the same as the first
// time s was masked on route under what o covers.
func (s *Scope) sentSpans(route, text string, o Options, find func() []span) []span {
	if s == nil || len(text) < minKnown {
		return find()
	}
	h := sha256.New()
	fmt.Fprintf(h, "%q\x00%t\x00%t\x00", route, o.Secrets, o.Personal)
	kinds := make([]string, 0, len(o.Kinds))
	for k, on := range o.Kinds {
		kinds = append(kinds, fmt.Sprintf("%q=%t", k, on))
	}
	slices.Sort(kinds)
	fmt.Fprintf(h, "%s\x00", strings.Join(kinds, ","))
	h.Write([]byte(text))
	var k [32]byte
	h.Sum(k[:0])
	s.mu.Lock()
	sp, ok := s.sentCur[k]
	if !ok {
		if sp, ok = s.sentOld[k]; ok {
			s.sentAdd(k, sp) // still in use: kept past the next turnover
		}
	}
	s.mu.Unlock()
	if ok {
		return slices.Clone(sp)
	}
	sp = find()
	s.mu.Lock()
	s.sentAdd(k, slices.Clip(slices.Clone(sp)))
	s.mu.Unlock()
	return sp
}

// sentAdd is called with s.mu held.
func (s *Scope) sentAdd(k [32]byte, sp []span) {
	if s.sentCur == nil || len(s.sentCur) >= sentEntries {
		s.sentOld, s.sentCur = s.sentCur, make(map[[32]byte][]span)
	}
	s.sentCur[k] = sp
}

// Scopes holds the scopes of a process by id.
type Scopes struct {
	mu  sync.Mutex
	m   map[string]*Scope
	now func() time.Time
}

// NewScopes returns an empty set of scopes.
func NewScopes() *Scopes {
	return &Scopes{m: make(map[string]*Scope), now: time.Now}
}

// Get returns the scope of id, made when there is none.
func (x *Scopes) Get(id string) *Scope {
	if x == nil {
		return nil
	}
	now := x.now()
	x.mu.Lock()
	defer x.mu.Unlock()
	if s, ok := x.m[id]; ok && now.Sub(s.last) < scopeIdle {
		s.last = now
		return s
	}
	if len(x.m) >= maxScopes {
		x.prune(now)
	}
	s := newScope(now)
	x.m[id] = s
	return s
}

// Drop forgets every scope whose id starts with prefix (a tenant that turned
// masking off, or changed its key).
func (x *Scopes) Drop(prefix string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for id := range x.m {
		if strings.HasPrefix(id, prefix) {
			delete(x.m, id)
		}
	}
}

// prune lets the idle scopes go and, if that is not enough, the half used
// longest ago; called with x.mu held.
func (x *Scopes) prune(now time.Time) {
	for id, s := range x.m {
		if now.Sub(s.last) >= scopeIdle {
			delete(x.m, id)
		}
	}
	if len(x.m) < maxScopes {
		return
	}
	ids := make([]string, 0, len(x.m))
	for id := range x.m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return x.m[ids[i]].last.Before(x.m[ids[j]].last) })
	for _, id := range ids[:len(ids)/2] {
		delete(x.m, id)
	}
}

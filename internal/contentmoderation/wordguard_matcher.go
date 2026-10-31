package contentmoderation

import (
	"hash/maphash"
	"sort"
	"sync"
	"unicode/utf8"
)

// wordRuleRef is a rule together with the policy layer it came from, which is
// what logs and the request log report.
type wordRuleRef struct {
	WordRule
	Layer string
}

// wordHit is a match of the rules in a normalized text.
type wordHit struct {
	// start and end are byte offsets in the normalized text.
	start, end int
	// rule indexes wordMatcher.rules. When rules overlap the hit is their union
	// and rule is the one that began first (the longest, on a tie).
	rule int
}

// wordFind is the outcome of scanning one text.
type wordFind struct {
	// hits are ascending and do not overlap.
	hits []wordHit
	// undecided is the offset of the first character that may still become the
	// start of a match once more text follows, or -1. It is only ever set when
	// the scan was told more text may come, and it is what the reply filter holds
	// back across stream events.
	undecided int
}

// wordMatcher is a context's rules compiled for matching. It is immutable and
// shared between requests.
type wordMatcher struct {
	fold  bool
	rules []wordRuleRef // enabled rules in the order they were given
	trie  wordTrie
	// regexes pairs each compiled expression with its index in rules.
	regexes []compiledWordRegex
}

// findOptions tunes a scan.
type findOptions struct {
	// more says the text may be followed by more of the same stream, so a match
	// that could still grow must not be reported yet.
	more bool
	// limit stops the scan after that many hits. Zero means no limit.
	limit int
	// budget is charged for every regular expression run over the text, and an
	// expression it cannot pay for is not run (see regexBudget). Nil means the
	// scan is not limited, which only tests rely on: a request or a reply always
	// brings its own.
	budget *regexBudget
}

// find scans normalized text for the matcher's rules.
//
// Keywords are matched leftmost-longest: of the keywords that start at the
// first matching position the longest wins, and scanning resumes after it.
// Regular expressions are compiled leftmost-longest as well. The two result
// lists are merged; hits that overlap become their union, so no part of a
// banned expression survives a replacement just because a shorter rule
// matched part of it first.
func (m *wordMatcher) find(text string, options findOptions) wordFind {
	result := wordFind{undecided: -1}
	if m == nil || text == "" {
		return result
	}
	hits, undecided := m.trie.scan(text, options)
	result.undecided = undecided
	for _, compiled := range m.regexes {
		if options.limit > 0 && len(hits) >= options.limit && !options.more {
			break
		}
		if !options.budget.take(int64(compiled.program) * int64(len(text))) {
			continue // fail open: this expression sees none of this text
		}
		// With a limit and no more text to come, only the first few matches can
		// matter; a text made of nothing but matches should not build a list of
		// them all.
		count := -1
		if options.limit > 0 && !options.more {
			count = options.limit
		}
		for _, loc := range compiled.re.FindAllStringIndex(text, count) {
			if loc[1] <= loc[0] {
				continue // validated away at save time; never act on an empty match
			}
			if options.more && loc[1] == len(text) {
				// A match that runs to the end of the text may be the beginning of
				// a longer one. RE2 cannot say whether a text is the start of a
				// match, so this is as much as can be held back for a regex: a
				// match that stops short of the end, or a prefix that is not yet a
				// match, is not.
				if result.undecided < 0 || loc[0] < result.undecided {
					result.undecided = loc[0]
				}
				continue
			}
			hits = append(hits, wordHit{start: loc[0], end: loc[1], rule: compiled.rule})
		}
	}
	result.hits = mergeWordHits(hits)
	if result.undecided >= 0 {
		// The keyword scan and the expressions each found their own undecided
		// start without knowing the other's hits. A final hit that straddles the
		// offset would be cut in two by the hold-back, so the hold-back starts at
		// that hit instead: it is matched again, whole, with the next text.
		for i := len(result.hits) - 1; i >= 0; i-- {
			if hit := result.hits[i]; hit.start < result.undecided && hit.end > result.undecided {
				result.undecided = hit.start
			}
		}
		// Matches that begin at or after the undecided offset are not final.
		kept := result.hits[:0]
		for _, hit := range result.hits {
			if hit.start < result.undecided {
				kept = append(kept, hit)
			}
		}
		result.hits = kept
	}
	if options.limit > 0 && len(result.hits) > options.limit {
		result.hits = result.hits[:options.limit]
	}
	return result
}

// mergeWordHits orders hits and joins the ones that overlap.
func mergeWordHits(hits []wordHit) []wordHit {
	if len(hits) < 2 {
		return hits
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return hits[i].end > hits[j].end
	})
	merged := hits[:1]
	for _, hit := range hits[1:] {
		last := &merged[len(merged)-1]
		if hit.start < last.end {
			if hit.end > last.end {
				last.end = hit.end
			}
			continue
		}
		merged = append(merged, hit)
	}
	return merged
}

// wordMatcherCache keeps compiled matchers off the per-request path: rules are
// read from the store on every request, but a list is only compiled the first
// time it is seen, keyed by its exact content.
var wordMatcherCache = struct {
	mu       sync.Mutex
	seed     maphash.Seed
	matchers map[uint64]*wordMatcher
}{seed: maphash.MakeSeed(), matchers: make(map[uint64]*wordMatcher)}

// maxCachedWordMatchers bounds the cache. Each distinct rule list takes one
// entry, so the bound is only reached by an unusual number of policies; the
// cache then starts over rather than grow.
const maxCachedWordMatchers = 512

// compiledWordMatcher returns the matcher for a rule list, compiling it if no
// identical list was compiled before. A list without an enabled rule has no
// matcher (nil).
func compiledWordMatcher(refs []wordRuleRef, fold bool) *wordMatcher {
	enabled := make([]wordRuleRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Enabled && ref.Pattern != "" {
			enabled = append(enabled, ref)
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	fingerprint := wordRulesFingerprint(enabled, fold)
	wordMatcherCache.mu.Lock()
	cached := wordMatcherCache.matchers[fingerprint]
	wordMatcherCache.mu.Unlock()
	if cached != nil && cached.compiledFrom(enabled, fold) {
		return cached
	}
	matcher := compileWordMatcher(enabled, fold)
	wordMatcherCache.mu.Lock()
	if len(wordMatcherCache.matchers) >= maxCachedWordMatchers {
		wordMatcherCache.matchers = make(map[uint64]*wordMatcher)
	}
	wordMatcherCache.matchers[fingerprint] = matcher
	wordMatcherCache.mu.Unlock()
	return matcher
}

func wordRulesFingerprint(refs []wordRuleRef, fold bool) uint64 {
	var hash maphash.Hash
	hash.SetSeed(wordMatcherCache.seed)
	if fold {
		_ = hash.WriteByte(1)
	} else {
		_ = hash.WriteByte(0)
	}
	for _, ref := range refs {
		_, _ = hash.WriteString(ref.Layer)
		_ = hash.WriteByte(0)
		_, _ = hash.WriteString(ref.ID)
		_ = hash.WriteByte(0)
		_, _ = hash.WriteString(ref.Kind)
		_ = hash.WriteByte(0)
		_, _ = hash.WriteString(ref.Pattern)
		_ = hash.WriteByte(1)
	}
	return hash.Sum64()
}

func (m *wordMatcher) compiledFrom(refs []wordRuleRef, fold bool) bool {
	if m.fold != fold || len(m.rules) != len(refs) {
		return false
	}
	for i, ref := range refs {
		if m.rules[i] != ref {
			return false
		}
	}
	return true
}

// compileWordMatcher builds a matcher from enabled rules. A rule that cannot be
// compiled (a policy stored by an older build with looser validation) is
// skipped rather than failing the request.
func compileWordMatcher(refs []wordRuleRef, fold bool) *wordMatcher {
	matcher := &wordMatcher{fold: fold, rules: append([]wordRuleRef(nil), refs...)}
	var builder wordTrieBuilder
	for index, ref := range matcher.rules {
		switch ref.Kind {
		case WordRuleRegex:
			if compiled, code, _ := compileWordRegex(ref.Pattern); code == "" {
				compiled.rule = index
				matcher.regexes = append(matcher.regexes, compiled)
			}
		default:
			if normalized := normalizeForMatch(ref.Pattern, fold); normalized != "" {
				builder.add(normalized, index)
			}
		}
	}
	matcher.trie = builder.build()
	return matcher
}

// wordTrie is the keyword set as a byte trie, flattened into two slices.
type wordTrie struct {
	nodes []trieNode
	edges []trieEdge
	// root maps the first byte of a keyword to the node reached, so that the
	// scan skips a position at the cost of one array read.
	root [256]int32
}

type trieNode struct {
	first int32 // index of the node's first edge in edges
	count int32 // number of edges
	// term is the rule index plus one when a keyword ends at this node.
	term int32
}

type trieEdge struct {
	b  byte
	to int32
}

// wordTrieBuilder collects keywords and builds a wordTrie. Of two keywords
// that normalize to the same text the one added first wins, which is the first
// in the rule list.
type wordTrieBuilder struct {
	words []builderWord
}

type builderWord struct {
	text string
	rule int
}

func (b *wordTrieBuilder) add(text string, rule int) {
	b.words = append(b.words, builderWord{text: text, rule: rule})
}

func (b *wordTrieBuilder) build() wordTrie {
	var trie wordTrie
	if len(b.words) == 0 {
		return trie
	}
	// A stable sort keeps duplicates in rule order, so the first one claims the
	// node and later ones are ignored.
	sort.SliceStable(b.words, func(i, j int) bool { return b.words[i].text < b.words[j].text })
	type buildNode struct {
		edges []trieEdge
		term  int32
	}
	nodes := []buildNode{{}} // node 0 is the root
	path := []int32{0}       // path[d] is the node at depth d of the previous word
	previous := ""
	for _, word := range b.words {
		common := 0
		for common < len(previous) && common < len(word.text) && previous[common] == word.text[common] {
			common++
		}
		path = path[:common+1]
		for depth := common; depth < len(word.text); depth++ {
			child := int32(len(nodes))
			nodes = append(nodes, buildNode{})
			parent := path[depth]
			nodes[parent].edges = append(nodes[parent].edges, trieEdge{b: word.text[depth], to: child})
			path = append(path, child)
		}
		if end := path[len(word.text)]; nodes[end].term == 0 {
			nodes[end].term = int32(word.rule) + 1
		}
		previous = word.text
	}
	trie.nodes = make([]trieNode, len(nodes))
	for i, node := range nodes {
		trie.nodes[i] = trieNode{first: int32(len(trie.edges)), count: int32(len(node.edges)), term: node.term}
		trie.edges = append(trie.edges, node.edges...)
	}
	for _, edge := range nodes[0].edges {
		trie.root[edge.b] = edge.to
	}
	return trie
}

// child returns the node reached from node by byte b, or 0 (the root, which no
// edge leads to) when there is none.
func (t *wordTrie) child(node int32, b byte) int32 {
	n := t.nodes[node]
	edges := t.edges[n.first : n.first+n.count]
	if len(edges) <= 8 {
		for _, edge := range edges {
			if edge.b == b {
				return edge.to
			}
		}
		return 0
	}
	i := sort.Search(len(edges), func(i int) bool { return edges[i].b >= b })
	if i < len(edges) && edges[i].b == b {
		return edges[i].to
	}
	return 0
}

// scan finds keyword hits, leftmost-longest. With options.more it stops at the
// first position that could still turn into a match given more text and
// returns that offset; hits before it are final.
func (t *wordTrie) scan(text string, options findOptions) ([]wordHit, int) {
	if len(t.nodes) == 0 {
		return nil, -1
	}
	var hits []wordHit
	for p := 0; p < len(text); {
		first := t.root[text[p]]
		if first == 0 {
			p += runeLen(text, p)
			continue
		}
		node, q := first, p+1
		bestEnd, bestTerm := -1, int32(0)
		if term := t.nodes[node].term; term != 0 {
			bestEnd, bestTerm = q, term
		}
		for q < len(text) {
			next := t.child(node, text[q])
			if next == 0 {
				break
			}
			node = next
			q++
			if term := t.nodes[node].term; term != 0 {
				bestEnd, bestTerm = q, term
			}
		}
		if q == len(text) && options.more && t.nodes[node].count > 0 {
			// The text ends inside a keyword that could still be completed (or
			// extended to a longer one). Nothing from p on is final.
			return hits, p
		}
		if bestEnd >= 0 {
			hits = append(hits, wordHit{start: p, end: bestEnd, rule: int(bestTerm) - 1})
			if options.limit > 0 && len(hits) >= options.limit {
				return hits, -1
			}
			p = bestEnd
			continue
		}
		p += runeLen(text, p)
	}
	return hits, -1
}

// runeLen is the byte length of the character at text[p]; an invalid byte
// counts as one so the scan always advances.
func runeLen(text string, p int) int {
	if text[p] < utf8.RuneSelf {
		return 1
	}
	_, size := utf8.DecodeRuneInString(text[p:])
	return size
}

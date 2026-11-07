package contentmoderation

import (
	"hash/maphash"
	"sort"
	"strings"
	"sync"
)

// keywordEntry pairs a configured keyword with its normalized form. Matching
// uses the normalized form; decisions and logs report the original, so an
// operator sees the word as it appears in their list.
type keywordEntry struct {
	original   string
	normalized string
}

// keywordSet is a profile's keyword list compiled for matching.
type keywordSet struct {
	foldTraditional bool
	// source is the list the set was compiled from. A cache hit is confirmed
	// against it, so a fingerprint collision can never apply the wrong list.
	source  []string
	entries []keywordEntry
}

// maxCachedKeywordSets bounds the compiled-list cache. Each profile version
// with a distinct list takes one entry, so the bound is only reached by an
// unusual number of profiles; the cache then starts over rather than grow.
const maxCachedKeywordSets = 1024

// keywordSetCache keeps keyword normalization off the per-request path:
// profiles are re-read from the store on every request, but a list is only
// normalized the first time it is seen, keyed by its exact content.
var keywordSetCache = struct {
	mu   sync.Mutex
	seed maphash.Seed
	sets map[uint64]*keywordSet
}{seed: maphash.MakeSeed(), sets: make(map[uint64]*keywordSet)}

// profileKeywordSet returns the profile's keyword list compiled for matching.
func profileKeywordSet(profile Profile) *keywordSet {
	return compiledKeywordSet(profile.BlockedKeywords, profile.KeywordFoldTraditionalChinese)
}

func compiledKeywordSet(keywords []string, foldTraditional bool) *keywordSet {
	if len(keywords) == 0 {
		return nil
	}
	fingerprint := keywordFingerprint(keywords, foldTraditional)
	keywordSetCache.mu.Lock()
	cached := keywordSetCache.sets[fingerprint]
	keywordSetCache.mu.Unlock()
	if cached != nil && cached.compiledFrom(keywords, foldTraditional) {
		return cached
	}
	set := compileKeywordSet(keywords, foldTraditional)
	keywordSetCache.mu.Lock()
	if len(keywordSetCache.sets) >= maxCachedKeywordSets {
		keywordSetCache.sets = make(map[uint64]*keywordSet)
	}
	keywordSetCache.sets[fingerprint] = set
	keywordSetCache.mu.Unlock()
	return set
}

func keywordFingerprint(keywords []string, foldTraditional bool) uint64 {
	var hash maphash.Hash
	hash.SetSeed(keywordSetCache.seed)
	if foldTraditional {
		_ = hash.WriteByte(1)
	} else {
		_ = hash.WriteByte(0)
	}
	for _, keyword := range keywords {
		_, _ = hash.WriteString(keyword)
		_ = hash.WriteByte(0)
	}
	return hash.Sum64()
}

func (s *keywordSet) compiledFrom(keywords []string, foldTraditional bool) bool {
	if s.foldTraditional != foldTraditional || len(s.source) != len(keywords) {
		return false
	}
	for i, keyword := range keywords {
		if s.source[i] != keyword {
			return false
		}
	}
	return true
}

// compileKeywordSet normalizes a keyword list. A keyword that normalizes to
// nothing (one made only of zero-width characters, say) is dropped: as an
// empty substring it would match every request. Keywords that normalize to the
// same text keep only the first, which is what the first-match scan reports
// anyway.
func compileKeywordSet(keywords []string, foldTraditional bool) *keywordSet {
	set := &keywordSet{
		foldTraditional: foldTraditional,
		source:          append([]string(nil), keywords...),
		entries:         make([]keywordEntry, 0, len(keywords)),
	}
	seen := make(map[string]struct{}, len(keywords))
	for _, keyword := range keywords {
		original := strings.TrimSpace(keyword)
		normalized := normalizeForMatch(original, foldTraditional)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		set.entries = append(set.entries, keywordEntry{original: original, normalized: normalized})
	}
	return set
}

// keywordText is a request's moderatable text in normalized form, every class
// included, segments joined by single spaces in conversation order. Joining
// keeps a keyword split across two adjacent blocks detectable, as before.
type keywordText struct {
	text string
	// starts[i] is the byte offset in text where segment i begins.
	starts  []int
	classes []Class
}

func buildKeywordText(segments []Segment, foldTraditional bool) *keywordText {
	out := &keywordText{starts: make([]int, 0, len(segments)), classes: make([]Class, 0, len(segments))}
	var b strings.Builder
	for _, segment := range segments {
		normalized := normalizeForMatch(segment.Text, foldTraditional)
		if normalized == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		out.starts = append(out.starts, b.Len())
		out.classes = append(out.classes, segment.Class)
		b.WriteString(normalized)
	}
	out.text = b.String()
	return out
}

// classAt returns the class of the segment containing byte offset.
func (k *keywordText) classAt(offset int) Class {
	i := sort.Search(len(k.starts), func(i int) bool { return k.starts[i] > offset }) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(k.classes) {
		return ClassPrimary
	}
	return k.classes[i]
}

type keywordHit struct {
	keyword string
	class   Class
	matched bool
}

// matchKeywords scans the input for the first keyword of the set, in list
// order. The normalized text and the verdict are memoized on the input.
func (in Input) matchKeywords(set *keywordSet) keywordHit {
	if set == nil || len(set.entries) == 0 || in.Empty() {
		return keywordHit{}
	}
	if in.memo == nil {
		return scanKeywords(buildKeywordText(in.Segments, set.foldTraditional), set)
	}
	in.memo.mu.Lock()
	defer in.memo.mu.Unlock()
	if in.memo.lastSet == set {
		return in.memo.lastHit
	}
	slot := 0
	if set.foldTraditional {
		slot = 1
	}
	text := in.memo.texts[slot]
	if text == nil {
		text = buildKeywordText(in.Segments, set.foldTraditional)
		in.memo.texts[slot] = text
	}
	hit := scanKeywords(text, set)
	in.memo.lastSet, in.memo.lastHit = set, hit
	return hit
}

func scanKeywords(text *keywordText, set *keywordSet) keywordHit {
	for _, entry := range set.entries {
		if offset := strings.Index(text.text, entry.normalized); offset >= 0 {
			return keywordHit{keyword: entry.original, class: text.classAt(offset), matched: true}
		}
	}
	return keywordHit{}
}

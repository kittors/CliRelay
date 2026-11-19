package util

import (
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	parametersJSONSchemaKey = "parametersJsonSchema"
	parametersKey           = "parameters"
)

// CleanAntigravityRequestSchemas renames every "parametersJsonSchema" key in an
// Antigravity request to "parameters" and cleans the JSON schemas in it: the
// Antigravity cleanup when addPlaceholder is set (the Claude family and
// gemini-3-pro-high), the Gemini one otherwise.
//
// The result is byte for byte what renaming each of those keys with RenameKey, in
// Walk order, and then running CleanJSONSchemaForAntigravity or
// CleanJSONSchemaForGemini over the whole payload returns. That is what
// buildRequest used to do, and it rewrote the whole payload, screenshots included,
// once per edit: a 5.4 MB request with forty raw tool schemas allocated 25 GB, and
// on 2026-11-19 it was 60% of everything the relay allocated.
//
// Every edit the cleanup makes stays close to a key it looked up, so this runs the
// same code on the smallest subtrees holding such keys, each wrapped in a skeleton
// that keeps its real path, and splices the results back in a single pass. A payload
// it cannot vouch for goes through the whole-payload path, which is the old code.
//
// That includes keys the cleanup was never meant for: function call arguments in
// request.contents are cleaned as if they were schemas (a replayed Grep call loses
// its "pattern"). That is a defect of the cleanup, kept here on purpose so that this
// change moves no byte; fixing it is a behaviour change of its own.
func CleanAntigravityRequestSchemas(payload string, addPlaceholder bool) string {
	if out, ok := renameAndCleanScoped(payload, addPlaceholder); ok {
		return out
	}
	return renameAndCleanWholePayload(payload, addPlaceholder)
}

// renameAndCleanWholePayload is what buildRequest ran before: it is the reference
// the scoped path is held to, and its fallback.
func renameAndCleanWholePayload(payload string, addPlaceholder bool) string {
	paths := make([]string, 0)
	Walk(gjson.Parse(payload), "", parametersJSONSchemaKey, &paths)
	for _, p := range paths {
		// RenameKey returns "" when the key is gone, which happens when one
		// parametersJsonSchema sits inside another; the scoped path never gets
		// here with such a payload, and the old behaviour is kept for it.
		payload, _ = RenameKey(payload, p, p[:len(p)-len(parametersJSONSchemaKey)]+parametersKey)
	}
	return cleanJSONSchema(payload, addPlaceholder)
}

// scopeKeys are the keys the cleanup (and the rename ahead of it) looks up
// anywhere in a document. "type" is looked up too, but only some of its values
// lead to an edit, so isScopeKey decides it separately. Any new key a cleanup pass
// looks up has to be added here, or the scoped path will skip the subtrees that
// hold it; TestScopeKeysCoverEveryKeyTheCleanupEdits catches a missing one.
var scopeKeys = func() map[string]struct{} {
	keys := []string{
		parametersJSONSchemaKey,
		"$ref", "const", "enum", "additionalProperties", "allOf", "anyOf", "oneOf", "required",
		"$schema", "$defs", "definitions", "$id", "propertyNames", "patternProperties", "enumTitles", "prefill",
		"nullable", "title", "_", "reason",
	}
	keys = append(keys, unsupportedConstraints...)
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}()

// isScopeKeyName reports whether a key of this name can lead the cleanup to an
// edit for some value.
func isScopeKeyName(key string) bool {
	if _, ok := scopeKeys[key]; ok {
		return true
	}
	return key == "type" || strings.HasPrefix(key, "x-")
}

// isScopeKey reports whether this key, holding this value, can lead the cleanup to
// an edit. A "type" leads to one only as a list (flattenTypeArrays), as "object"
// (addEmptySchemaPlaceholder) or as "array" in any case (addMissingArrayItems);
// the far more common "type": "string" in function call arguments does not.
func isScopeKey(key string, value gjson.Result) bool {
	if key != "type" {
		return isScopeKeyName(key)
	}
	if value.IsArray() {
		return true
	}
	return value.Type == gjson.String && (value.Str == "object" || strings.EqualFold(value.Str, "array"))
}

// scopeSeg is one step of a path: an object key or an array index, with the span
// of the value it leads to.
type scopeSeg struct {
	key        string
	index      int    // -1 for an object key
	raw        string // the value, as a slice of the payload
	start, end int    // the value's span in the payload
	object     bool
}

func (s scopeSeg) is(key string) bool { return s.index < 0 && s.key == key }

// safe reports whether the step can stand in a skeleton: it is written there as a
// JSON key and a path part as is, so it must need no escaping in either. Steps that
// fail this sit inside a unit instead.
func (s scopeSeg) safe() bool {
	if s.index >= 0 {
		return true
	}
	if s.key == "" {
		return false
	}
	for i := 0; i < len(s.key); i++ {
		c := s.key[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// scopeUnit is a subtree the cleanup runs on by itself: path leads to it (the
// skeleton), start and end are its span in the payload.
type scopeUnit struct {
	start, end int
	path       []scopeSeg
	cleaned    string
}

type scopeScan struct {
	segs     []scopeSeg
	units    []scopeUnit
	keys     []string // keys of the objects being walked, to spot duplicates
	risky    []int    // start of every value under a key the cleanup can misread
	fallback bool
}

// aliasFree reports whether a key reaches only itself when the cleanup turns it
// into a path. The cleanup escapes ".", "*" and "?" but nothing else, so a key
// holding "|", "#", "@", "\" and the like resolves to some other node ("pipe|name"
// reads the sibling "pipe", piped into "name"), and an empty key to none.
//
// Two paths that reach the same node break the one thing the scoped path relies
// on beyond locality: that edits of paths of equal length commute. The cleanup
// orders its edits with an unstable sort by path length, and the order it ends up
// with depends on every path in the document, so the scoped path cannot reproduce
// it; it can only rely on the order not mattering.
func aliasFree(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c >= 0x80:
		case c == '_', c == '-', c == '$', c == '.', c == '/', c == ' ', c == '*', c == '?':
		default:
			return false
		}
	}
	return true
}

func renameAndCleanScoped(payload string, addPlaceholder bool) (string, bool) {
	// Offsets come from the parse below, and they are only exact on valid JSON.
	if !gjson.Valid(payload) {
		return "", false
	}
	root := gjson.Parse(payload)
	if !root.IsObject() {
		return "", false
	}
	var scan scopeScan
	scan.walk(root, 0)
	if scan.fallback {
		return "", false
	}
	if len(scan.units) == 0 {
		// Nothing the cleanup looks for is present, and with nothing found it
		// returns its input unchanged.
		return payload, true
	}
	units := mergeScopeUnits(scan.units)
	for _, u := range units {
		// gjson reports offsets into the payload it parsed (leading whitespace
		// included); make sure each span really is the value the scan saw.
		if u.end > len(payload) || payload[u.start:u.end] != u.path[len(u.path)-1].raw {
			return "", false
		}
	}
	if anyInsideUnit(scan.risky, units) {
		return "", false
	}
	size := len(payload)
	for i := range units {
		cleaned, ok := cleanScopeUnit(units[i], addPlaceholder)
		if !ok {
			return "", false
		}
		units[i].cleaned = cleaned
		size += len(cleaned) - (units[i].end - units[i].start)
	}
	var out strings.Builder
	out.Grow(size)
	prev := 0
	for _, u := range units {
		out.WriteString(payload[prev:u.start])
		out.WriteString(u.cleaned)
		prev = u.end
	}
	out.WriteString(payload[prev:])
	return out.String(), true
}

func (s *scopeScan) walk(node gjson.Result, depth int) {
	isArray := node.IsArray()
	index := -1
	keysBase := len(s.keys)
	var manyKeys map[string]struct{}
	node.ForEach(func(key, value gjson.Result) bool {
		seg := scopeSeg{
			index:  -1,
			raw:    value.Raw,
			start:  value.Index,
			end:    value.Index + len(value.Raw),
			object: value.IsObject(),
		}
		if isArray {
			index++
			seg.index = index
		} else {
			seg.key = key.String()
			// Paths name the first of two equal keys, so a duplicate makes the
			// cleanup edit one node twice and the other never. The entrypoints
			// reject such bodies (#1267); anything else gets the old code.
			if !s.noteKey(keysBase, &manyKeys, seg.key) {
				s.fallback = true
				return false
			}
			if !aliasFree(seg.key) {
				s.risky = append(s.risky, seg.start)
			}
		}
		s.segs = append(s.segs[:depth], seg)
		if !isArray && isScopeKey(seg.key, value) {
			s.addUnit(depth + 1)
		}
		if !s.fallback && (value.IsObject() || value.IsArray()) {
			s.walk(value, depth+1)
		}
		return !s.fallback
	})
	s.keys = s.keys[:keysBase]
}

// noteKey records a key of the object whose keys start at s.keys[base] and reports
// false if the object already had it. Small objects are searched linearly; one
// that grows past a handful of keys gets a set.
func (s *scopeScan) noteKey(base int, many *map[string]struct{}, key string) bool {
	if *many != nil {
		if _, dup := (*many)[key]; dup {
			return false
		}
		(*many)[key] = struct{}{}
		return true
	}
	for _, prev := range s.keys[base:] {
		if prev == key {
			return false
		}
	}
	s.keys = append(s.keys, key)
	if len(s.keys)-base > 16 {
		*many = make(map[string]struct{}, 2*(len(s.keys)-base))
		for _, prev := range s.keys[base:] {
			(*many)[prev] = struct{}{}
		}
	}
	return true
}

// anyInsideUnit reports whether any of the offsets falls inside one of the
// (sorted, disjoint) units.
func anyInsideUnit(offsets []int, units []scopeUnit) bool {
	for _, at := range offsets {
		i := sort.Search(len(units), func(i int) bool { return units[i].end > at })
		if i < len(units) && units[i].start <= at {
			return true
		}
	}
	return false
}

// addUnit records the unit that has to hold the scope key at the end of
// s.segs[:n].
func (s *scopeScan) addUnit(n int) {
	depth := scopeUnitDepth(s.segs[:n])
	if depth == 0 {
		// The key sits too close to the root to cut a subtree around it.
		s.fallback = true
		return
	}
	root := s.segs[depth-1]
	if last := len(s.units) - 1; last >= 0 && s.units[last].start <= root.start && root.end <= s.units[last].end {
		return // already inside the unit found for an earlier key
	}
	s.units = append(s.units, scopeUnit{
		start: root.start,
		end:   root.end,
		path:  append([]scopeSeg(nil), s.segs[:depth]...),
	})
}

// scopeUnitDepth returns the depth of the unit root for the scope key at the end
// of segs: the shortest prefix of segs whose subtree holds everything the cleanup
// may read or write on account of that key.
func scopeUnitDepth(segs []scopeSeg) int {
	depth := scopeReach(segs, len(segs))
	for {
		for i := 0; i < depth; i++ {
			if !segs[i].safe() {
				depth = i
				break
			}
		}
		// A unit root named like a scope key could be turned into one by the
		// cleanup itself: flattenAnyOfOneOf replaces the object holding an anyOf
		// with the chosen branch, so a "type": {"anyOf": ["object"]} becomes
		// "type": "object" and addEmptySchemaPlaceholder then edits its parent.
		if depth == 0 || segs[depth-1].index >= 0 || !isScopeKeyName(segs[depth-1].key) {
			return depth
		}
		depth = scopeReach(segs, depth)
	}
}

// scopeReach returns the depth of the highest node the cleanup touches on account
// of the key at segs[n-1], whose parent schema is the node at depth n-1.
//
//   - A schema that is a property definition (its parent is a "properties" map):
//     flattenTypeArrays drops a nullable property from the required list of the
//     schema holding that map, two levels up.
//   - A key that is a property name (its parent is a "properties" map):
//     removePlaceholderFields edits the required list next to that map, one level up.
//   - Anything else reads and writes the parent schema only.
func scopeReach(segs []scopeSeg, n int) int {
	if n >= 3 && segs[n-3].is("properties") {
		return n - 3
	}
	if n >= 2 && segs[n-2].is("properties") {
		return n - 2
	}
	return n - 1
}

// mergeScopeUnits sorts units by position and drops the ones nested in another,
// leaving disjoint subtrees.
func mergeScopeUnits(units []scopeUnit) []scopeUnit {
	sort.Slice(units, func(i, j int) bool {
		if units[i].start != units[j].start {
			return units[i].start < units[j].start
		}
		return units[i].end > units[j].end
	})
	merged := units[:0]
	for _, u := range units {
		if n := len(merged); n > 0 && u.start < merged[n-1].end {
			continue // nested in the previous unit
		}
		merged = append(merged, u)
	}
	return merged
}

// cleanScopeUnit runs the whole-payload cleanup on a document holding only the
// unit, under its real path (array steps become object keys, which every path the
// cleanup builds resolves the same way), and returns the unit as cleaned.
func cleanScopeUnit(u scopeUnit, addPlaceholder bool) (string, bool) {
	var prefix strings.Builder
	for _, seg := range u.path {
		prefix.WriteString(`{"`)
		if seg.index >= 0 {
			prefix.WriteString(strconv.Itoa(seg.index))
		} else {
			prefix.WriteString(seg.key) // safe: needs no escaping
		}
		prefix.WriteString(`":`)
	}
	head := prefix.String()
	tail := strings.Repeat("}", len(u.path))
	out := renameAndCleanWholePayload(head+u.path[len(u.path)-1].raw+tail, addPlaceholder)
	// The skeleton holds no scope key, so the cleanup cannot have edited it; check
	// rather than assume, and let anything unexpected take the whole-payload path.
	if len(out) < len(head)+len(tail) || !strings.HasPrefix(out, head) || !strings.HasSuffix(out, tail) {
		return "", false
	}
	cleaned := out[len(head) : len(out)-len(tail)]
	if !gjson.Valid(cleaned) {
		return "", false
	}
	return cleaned, true
}

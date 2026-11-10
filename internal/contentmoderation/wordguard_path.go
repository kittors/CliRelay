package contentmoderation

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

// When a hit cannot be rewritten the client is told which field it is in, so it
// can find what to change, and a masked request's record notes the field each
// value was first found in. The path is worked out from the request's bytes at
// the moment it is needed, so a request that is rewritten without trouble and
// has nothing to note never pays for it.
//
// Working out a path walks the document up to the place, and a request of
// megabytes is mostly screenshots in base64: the paths of many places are
// found in one walk (jsonPathsAt, unitFields), never in a walk per place. One
// walk per masked string took longer than all the rest of masking, and grew
// with the number of strings times the size of the request.

// maxJSONPathDepth bounds the walk. A request nested deeper than any real one
// gets no path rather than a deep recursion.
const maxJSONPathDepth = 256

type jsonPathStep struct {
	// lit is an object key's literal, quotes and escapes included. It is
	// decoded only when a path is written: most keys a walk crosses are in no
	// path it writes.
	lit   []byte
	index int
	isKey bool
}

type jsonPathFinder struct {
	data []byte
	path []jsonPathStep
	// offsets are the places wanted and paths[k] the path of offsets[k]. order
	// lists the indexes of offsets by offset, and next is the first of order the
	// walk has not reached.
	offsets []int
	paths   []string
	order   []int
	next    int
	// done says every place is behind the walk: it stops there.
	done bool
}

// jsonPathAt returns where the value that starts at offset sits in the JSON
// document data, as a path like messages[1].content[0].text. The path of an
// object key ends with the key. It returns "" if it cannot tell.
func jsonPathAt(data []byte, offset int) string {
	return jsonPathsAt(data, []int{offset})[0]
}

// jsonPathWalks, when set, is told of every walk for paths: a test counts them
// to see that the places of a request are found in one walk of each document,
// not in one walk per place.
var jsonPathWalks func()

// jsonPathsAt is jsonPathAt of each of offsets, worked out in one walk of data
// that stops at the last of them.
func jsonPathsAt(data []byte, offsets []int) []string {
	if jsonPathWalks != nil {
		jsonPathWalks()
	}
	finder := &jsonPathFinder{data: data, offsets: offsets, paths: make([]string, len(offsets)), order: make([]int, len(offsets))}
	for k := range finder.order {
		finder.order[k] = k
	}
	sort.SliceStable(finder.order, func(a, b int) bool { return offsets[finder.order[a]] < offsets[finder.order[b]] })
	finder.value(finder.skip(0), 0)
	return finder.paths
}

// joinJSONPath continues the path of a document with a path inside it.
func joinJSONPath(outer, inner string) string {
	switch {
	case outer == "":
		return inner
	case inner == "":
		return outer
	case strings.HasPrefix(inner, "["):
		return outer + inner
	}
	return outer + "." + inner
}

func formatJSONPath(path []jsonPathStep) string {
	var b strings.Builder
	for _, step := range path {
		if !step.isKey {
			b.WriteString("[" + strconv.Itoa(step.index) + "]")
			continue
		}
		key, _ := decodeJSONLiteral(step.lit)
		if isPlainPathKey(key) {
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(key)
			continue
		}
		b.WriteString("[" + strconv.Quote(key) + "]")
	}
	return b.String()
}

func isPlainPathKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
		if !letter && !(i > 0 && (c >= '0' && c <= '9' || c == '-')) {
			return false
		}
	}
	return true
}

// reached is called where a value or a key starts, at i. It writes the path of
// every wanted place up to i: the current path for a place at i, none for a
// place the walk passed without anything starting there. It reports whether
// every place is behind the walk.
func (f *jsonPathFinder) reached(i int) bool {
	for ; f.next < len(f.order); f.next++ {
		k := f.order[f.next]
		if f.offsets[k] > i {
			return false
		}
		if f.offsets[k] == i {
			f.paths[k] = formatJSONPath(f.path)
		}
	}
	return true
}

func (f *jsonPathFinder) skip(i int) int {
	for i < len(f.data) && (f.data[i] == ' ' || f.data[i] == '\t' || f.data[i] == '\n' || f.data[i] == '\r') {
		i++
	}
	return i
}

// stringEnd returns the offset just after the string literal that starts at i,
// or -1. A quote ends it unless it follows an odd run of backslashes, which
// escapes it. The quotes are looked for with IndexByte rather than byte by
// byte: most of a large request is base64, which has neither quotes nor
// backslashes, and is crossed at memory speed.
func (f *jsonPathFinder) stringEnd(i int) int {
	for j := i + 1; j < len(f.data); {
		q := bytes.IndexByte(f.data[j:], '"')
		if q < 0 {
			return -1
		}
		q += j
		backslashes := 0
		for k := q - 1; k > i && f.data[k] == '\\'; k-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
	return -1
}

// value walks the value that starts at i and returns the offset after it, or -1
// if the document is malformed. Once every place is found the walk stops, and
// a place it has not reached by then has no path.
func (f *jsonPathFinder) value(i, depth int) int {
	i = f.skip(i)
	if f.done || i >= len(f.data) || depth > maxJSONPathDepth {
		return -1
	}
	if f.done = f.reached(i); f.done {
		return -1
	}
	switch f.data[i] {
	case '"':
		return f.stringEnd(i)
	case '{':
		return f.object(i+1, depth)
	case '[':
		return f.array(i+1, depth)
	default:
		for i < len(f.data) && !strings.ContainsRune(",]} \t\r\n", rune(f.data[i])) {
			i++
		}
		return i
	}
}

func (f *jsonPathFinder) object(i, depth int) int {
	if i = f.skip(i); i < len(f.data) && f.data[i] == '}' {
		return i + 1
	}
	for {
		i = f.skip(i)
		if i >= len(f.data) || f.data[i] != '"' {
			return -1
		}
		end := f.stringEnd(i)
		if end < 0 {
			return -1
		}
		f.path = append(f.path, jsonPathStep{lit: f.data[i:end], isKey: true})
		if f.done = f.reached(i); f.done {
			return -1
		}
		if i = f.skip(end); i >= len(f.data) || f.data[i] != ':' {
			return -1
		}
		if i = f.value(i+1, depth+1); i < 0 {
			return -1
		}
		f.path = f.path[:len(f.path)-1]
		if i = f.skip(i); i >= len(f.data) {
			return -1
		}
		switch f.data[i] {
		case ',':
			i++
		case '}':
			return i + 1
		default:
			return -1
		}
	}
}

func (f *jsonPathFinder) array(i, depth int) int {
	if i = f.skip(i); i < len(f.data) && f.data[i] == ']' {
		return i + 1
	}
	for index := 0; ; index++ {
		f.path = append(f.path, jsonPathStep{index: index})
		if i = f.value(i, depth+1); i < 0 {
			return -1
		}
		f.path = f.path[:len(f.path)-1]
		if i = f.skip(i); i >= len(f.data) {
			return -1
		}
		switch f.data[i] {
		case ',':
			i++
		case ']':
			return i + 1
		default:
			return -1
		}
	}
}

// unitFields is unitField of each of units. Each document the units sit in is
// walked once for all of them, the request included, however many units sit
// in it: a unit in the arguments of a tool call needs the place of those
// arguments in the request as well as its own place in them.
func unitFields(units []wordUnit) []string {
	type place struct {
		doc    *wordDoc
		offset int
	}
	wanted := make(map[*wordDoc][]int)
	for _, unit := range units {
		for doc, offset := unit.doc, unit.lit0; doc != nil; doc, offset = doc.parent, doc.lit0 {
			wanted[doc] = append(wanted[doc], offset)
		}
	}
	paths := make(map[place]string)
	for doc, offsets := range wanted {
		for k, path := range jsonPathsAt(doc.data, offsets) {
			paths[place{doc, offsets[k]}] = path
		}
	}
	// docPath is wordDoc.path from the paths found.
	var docPath func(*wordDoc) string
	docPath = func(d *wordDoc) string {
		if d.parent == nil {
			return ""
		}
		return joinJSONPath(docPath(d.parent), paths[place{d.parent, d.lit0}])
	}
	fields := make([]string, len(units))
	for i, unit := range units {
		if unit.doc == nil {
			continue
		}
		field := joinJSONPath(docPath(unit.doc), paths[place{unit.doc, unit.lit0}])
		if field != "" && unit.kind == unitKey {
			field += " (an object key)"
		}
		fields[i] = field
	}
	return fields
}

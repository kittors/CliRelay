package contentmoderation

import (
	"strconv"
	"strings"
)

// When a hit cannot be rewritten the client is told which field it is in, so it
// can find what to change. The path is worked out from the request's bytes at
// the moment it is needed, so a request that is rewritten without trouble never
// pays for it.

// maxJSONPathDepth bounds the walk. A request nested deeper than any real one
// gets no path rather than a deep recursion.
const maxJSONPathDepth = 256

type jsonPathStep struct {
	key   string
	index int
	isKey bool
}

type jsonPathFinder struct {
	data   []byte
	target int
	path   []jsonPathStep
	found  bool
}

// jsonPathAt returns where the value that starts at offset sits in the JSON
// document data, as a path like messages[1].content[0].text. The path of an
// object key ends with the key. It returns "" if it cannot tell.
func jsonPathAt(data []byte, offset int) string {
	finder := &jsonPathFinder{data: data, target: offset}
	finder.value(finder.skip(0), 0)
	if !finder.found {
		return ""
	}
	return formatJSONPath(finder.path)
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
		switch {
		case !step.isKey:
			b.WriteString("[" + strconv.Itoa(step.index) + "]")
		case isPlainPathKey(step.key):
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(step.key)
		default:
			b.WriteString("[" + strconv.Quote(step.key) + "]")
		}
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

func (f *jsonPathFinder) skip(i int) int {
	for i < len(f.data) && (f.data[i] == ' ' || f.data[i] == '\t' || f.data[i] == '\n' || f.data[i] == '\r') {
		i++
	}
	return i
}

// stringEnd returns the offset just after the string literal that starts at i,
// or -1.
func (f *jsonPathFinder) stringEnd(i int) int {
	for j := i + 1; j < len(f.data); j++ {
		switch f.data[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// value walks the value that starts at i and returns the offset after it, or -1
// if the document is malformed. Once the target is found the walk stops and the
// path stays as it was.
func (f *jsonPathFinder) value(i, depth int) int {
	i = f.skip(i)
	if f.found || i >= len(f.data) || depth > maxJSONPathDepth {
		return -1
	}
	if i == f.target {
		f.found = true
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
		name, _ := decodeJSONLiteral(f.data[i:end])
		f.path = append(f.path, jsonPathStep{key: name, isKey: true})
		if i == f.target {
			f.found = true
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

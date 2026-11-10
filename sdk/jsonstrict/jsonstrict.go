// Package jsonstrict checks that a request body is a JSON text every parser
// reads the same way, so that what the gateway routes, bills, moderates and
// masks is what the upstream acts on.
//
// The gateway reads requests with gjson, which takes the first of two members
// of an object that share a name and reads nothing at all of a text it finds
// invalid, and the executors send a request in the client's own format on as it
// came. Parsers disagree on both. Python's json, pydantic, Node and Go's
// encoding/json take the last of two members with one name; protobuf's JSON
// parser (Google's APIs) appends the second list to the first and merges the
// second object into the first; Go's json/v2 and serde refuse the text; Python's
// json and pydantic read NaN and Infinity, which no other parser does. A request
// that leans on any of that reads one way here and another way upstream: a
// second "messages" goes past the word guard and secret masking, a second
// "model" past the model gate, and a NaN past everything, as the moderation
// extractor reads nothing of a text gjson calls invalid.
//
// So a body must be a JSON text as RFC 8259 defines it, with the one
// restriction of I-JSON (RFC 7493, section 2.3) that matters here: no object
// has two members with the same name, names compared after their escapes are
// decoded. Nesting is bounded as encoding/json bounds it.
//
// What is left as it is: the UTF-8 of string values (a Node client that cuts a
// string inside an emoji sends a lone surrogate escape, and refusing it would
// refuse Claude Code), the range of numbers, and member names that differ only
// in case (one name to Go's encoding/json decoding into a struct, two to
// everything else: telling the members a protocol defines from free-form data
// such as tool arguments needs a model of each format, and is not done here).
package jsonstrict

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxDepth bounds the nesting of arrays and objects, as encoding/json bounds it.
const MaxDepth = 10000

// Error says what is wrong with a body and where. It never holds a string
// value of the body; a member name, which the client wrote too, is cut short.
type Error struct {
	// Offset is the byte offset of the problem in the body.
	Offset int64
	// Path is where it is, as a JSONPath: "$" for the top level,
	// "$.messages[1]" for the second element of messages. For a repeated name,
	// it is the object the name is repeated in.
	Path string
	// Duplicate says the problem is a member name the object repeats, and Name
	// is that name ("" is a name too).
	Duplicate bool
	Name      string
	// Reason says what is wrong otherwise.
	Reason string
}

func (e *Error) Error() string {
	if e.Duplicate {
		return fmt.Sprintf("the object at %s has two members named %s (byte %d); a name may appear only once in an object",
			e.Path, strconv.Quote(clip(e.Name)), e.Offset)
	}
	return fmt.Sprintf("%s at %s (byte %d)", e.Reason, e.Path, e.Offset)
}

// IsDuplicate reports whether err is a member name an object repeats.
func IsDuplicate(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Duplicate
}

// Check reports whether data is one strict JSON text (see the package
// comment). The error is an *Error.
func Check(data []byte) error {
	c := newChecker()
	if err := c.feed(data); err != nil {
		return err
	}
	return c.finish()
}

// CheckReader is Check for a body read from r. It reads r to the end in small
// pieces and never holds the body: a large request spilled to disk stays
// there. An error of r itself is returned as it is, not as an *Error.
func CheckReader(r io.Reader) error {
	c := newChecker()
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if checkErr := c.feed(buf[:n]); checkErr != nil {
				return checkErr
			}
		}
		if err == io.EOF {
			return c.finish()
		}
		if err != nil {
			return err
		}
	}
}

const readChunk = 32 << 10

// maxShownName bounds a member name in an error: it is the client's own text,
// and may be long.
const maxShownName = 64

// clip cuts a name to maxShownName bytes, on a rune boundary.
func clip(name string) string {
	if len(name) <= maxShownName {
		return name
	}
	cut := maxShownName
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + "…"
}

// writePathName appends .name, or ['name'] for a name that is not an
// identifier, to a JSONPath.
func writePathName(b *strings.Builder, name string) {
	name = clip(name)
	identifier := name != ""
	for i := 0; i < len(name) && identifier; i++ {
		ch := name[i]
		identifier = ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || i > 0 && ch >= '0' && ch <= '9'
	}
	if identifier {
		b.WriteByte('.')
		b.WriteString(name)
		return
	}
	b.WriteString("['")
	for _, r := range name {
		switch {
		case r == '\'' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == utf8.RuneError:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("']")
}

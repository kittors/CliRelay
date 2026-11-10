package jsonstrict

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The checker is a byte-at-a-time state machine, so a body can be fed to it in
// pieces of any size (CheckReader) and every state survives a cut. Strings,
// which are most of a request (base64 images, long prompts), are read in runs.

type state uint8

const (
	stValue        state = iota // a value comes next
	stObjectFirst               // after '{': a name or '}'
	stObjectName                // after ',' in an object: a name
	stColon                     // after a name: ':'
	stObjectNext                // after a member: ',' or '}'
	stArrayFirst                // after '[': a value or ']'
	stArrayNext                 // after an element: ',' or ']'
	stString                    // in a string
	stEscape                    // after '\' in a string
	stUnicode                   // in the four digits of \uXXXX
	stNumMinus                  // after '-'
	stNumZero                   // after a leading '0'
	stNumInt                    // in the integer digits
	stNumDot                    // after '.'
	stNumFrac                   // in the fraction digits
	stNumExp                    // after 'e' or 'E'
	stNumExpSign                // after the exponent's sign
	stNumExpDigits              // in the exponent digits
	stLiteral                   // in true, false or null
	stDone                      // after the top-level value
)

// linearNames is how many names an object keeps in a list before they move to
// a set: most objects have a handful, where a list beats a map, but an object
// with many members must not cost the square of their number.
const linearNames = 16

type frame struct {
	object bool
	// index is, in an array, the index of the element being read.
	index int
	// name is, in an object, the name of the member being read; hasName says
	// there is one (there is none before the first name and after a comma).
	name    string
	hasName bool
	// namesFrom is where the object's names start in checker.names; set holds
	// them instead once there are more than linearNames.
	namesFrom int
	set       map[string]struct{}
}

type checker struct {
	state state
	stack []frame
	// names are the names of the open objects, innermost last.
	names []string
	// key says the string being read is a member name; keyBuf is that name
	// decoded so far, keyAt the offset of its opening quote.
	key    bool
	keyBuf []byte
	keyAt  int64
	// hex counts the digits of the \uXXXX being read and code holds its value;
	// high is a high surrogate escape in a name, waiting for its low half.
	hex  int
	code rune
	high rune
	// literal is the true, false or null being read, litPos how much of it.
	literal string
	litPos  int
	// offset is the body offset of the piece being fed.
	offset int64
}

func newChecker() *checker { return &checker{state: stValue} }

// plain marks the bytes a string holds as they are: anything but a quote, a
// backslash and the control characters, which must be escaped.
var plain = func() (t [256]bool) {
	for b := 0x20; b < 256; b++ {
		t[b] = b != '"' && b != '\\'
	}
	return t
}()

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func (c *checker) feed(p []byte) error {
	for i := 0; i < len(p); {
		b := p[i]
		switch c.state {
		case stString:
			j := i
			for j < len(p) && plain[p[j]] {
				j++
			}
			if c.key && j > i {
				c.flushHigh()
				c.keyBuf = append(c.keyBuf, p[i:j]...)
			}
			if i = j; i == len(p) {
				continue
			}
			switch p[i] {
			case '"':
				if err := c.endString(); err != nil {
					return err
				}
			case '\\':
				c.state = stEscape
			default:
				return c.fail(i, "unescaped control character in a string")
			}
			i++
		case stEscape:
			switch b {
			case 'u':
				c.state, c.hex, c.code = stUnicode, 0, 0
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				if c.key {
					c.flushHigh()
					c.keyBuf = append(c.keyBuf, unescaped(b))
				}
				c.state = stString
			default:
				return c.fail(i, "invalid escape in a string")
			}
			i++
		case stUnicode:
			v, ok := hexValue(b)
			if !ok {
				return c.fail(i, `invalid \u escape in a string`)
			}
			c.code = c.code<<4 | v
			if c.hex++; c.hex == 4 {
				if c.key {
					c.keyRune(c.code)
				}
				c.state = stString
			}
			i++
		case stValue, stArrayFirst:
			switch {
			case isSpace(b):
			case b == ']' && c.state == stArrayFirst:
				c.pop()
			default:
				if err := c.beginValue(i, b); err != nil {
					return err
				}
			}
			i++
		case stObjectFirst, stObjectName:
			switch {
			case isSpace(b):
			case b == '"':
				c.key, c.keyBuf, c.keyAt, c.high = true, c.keyBuf[:0], c.offset+int64(i), 0
				c.state = stString
			case b == '}' && c.state == stObjectFirst:
				c.pop()
			case b == '}':
				return c.fail(i, "a comma before '}'")
			default:
				return c.fail(i, "expected a member name")
			}
			i++
		case stColon:
			switch {
			case isSpace(b):
			case b == ':':
				c.state = stValue
			default:
				return c.fail(i, "expected ':' after a member name")
			}
			i++
		case stObjectNext:
			switch {
			case isSpace(b):
			case b == ',':
				c.stack[len(c.stack)-1].hasName = false
				c.state = stObjectName
			case b == '}':
				c.pop()
			default:
				return c.fail(i, "expected ',' or '}' after a member")
			}
			i++
		case stArrayNext:
			switch {
			case isSpace(b):
			case b == ',':
				c.stack[len(c.stack)-1].index++
				c.state = stValue
			case b == ']':
				c.pop()
			default:
				return c.fail(i, "expected ',' or ']' after an element")
			}
			i++
		case stDone:
			if !isSpace(b) {
				return c.fail(i, "data after the JSON value")
			}
			i++
		case stLiteral:
			if b != c.literal[c.litPos] {
				return c.fail(i, "invalid literal")
			}
			if c.litPos++; c.litPos == len(c.literal) {
				c.afterValue()
			}
			i++
		default:
			// A number. The byte that ends one is read again in the state after it.
			next, consumed, err := c.number(i, b)
			if err != nil {
				return err
			}
			c.state = next
			if consumed {
				i++
			}
		}
	}
	c.offset += int64(len(p))
	return nil
}

// number reads one byte of a number (RFC 8259: -?(0|[1-9][0-9]*)(.[0-9]+)?
// ([eE][+-]?[0-9]+)?). It returns the state after it, and whether the byte was
// part of the number: one that is not ends it, and is read again.
func (c *checker) number(i int, b byte) (state, bool, error) {
	digit := isDigit(b)
	switch c.state {
	case stNumMinus:
		switch {
		case b == '0':
			return stNumZero, true, nil
		case digit:
			return stNumInt, true, nil
		}
	case stNumZero, stNumInt, stNumFrac:
		switch {
		case digit && c.state == stNumZero:
			return 0, false, c.fail(i, "invalid number: a leading zero")
		case digit:
			return c.state, true, nil
		case b == '.' && c.state != stNumFrac:
			return stNumDot, true, nil
		case b == 'e' || b == 'E':
			return stNumExp, true, nil
		}
		return c.valueEnded(), false, nil
	case stNumDot:
		if digit {
			return stNumFrac, true, nil
		}
	case stNumExp:
		switch {
		case b == '+' || b == '-':
			return stNumExpSign, true, nil
		case digit:
			return stNumExpDigits, true, nil
		}
	case stNumExpSign:
		if digit {
			return stNumExpDigits, true, nil
		}
	case stNumExpDigits:
		if digit {
			return stNumExpDigits, true, nil
		}
		return c.valueEnded(), false, nil
	}
	return 0, false, c.fail(i, "invalid number")
}

func (c *checker) beginValue(i int, b byte) error {
	switch {
	case b == '{', b == '[':
		if len(c.stack) >= MaxDepth {
			return c.fail(i, "arrays and objects nested too deeply")
		}
		c.stack = append(c.stack, frame{object: b == '{', namesFrom: len(c.names)})
		c.state = stArrayFirst
		if b == '{' {
			c.state = stObjectFirst
		}
	case b == '"':
		c.key, c.state = false, stString
	case b == '-':
		c.state = stNumMinus
	case b == '0':
		c.state = stNumZero
	case isDigit(b):
		c.state = stNumInt
	case b == 't':
		c.literal, c.litPos, c.state = "true", 1, stLiteral
	case b == 'f':
		c.literal, c.litPos, c.state = "false", 1, stLiteral
	case b == 'n':
		c.literal, c.litPos, c.state = "null", 1, stLiteral
	default:
		// NaN, Infinity, a single quote, a byte order mark...
		return c.fail(i, "invalid character "+describe(b)+" where a value should be")
	}
	return nil
}

// valueEnded is the state after a value: what the enclosing array or object
// takes next, or nothing at the top.
func (c *checker) valueEnded() state {
	switch {
	case len(c.stack) == 0:
		return stDone
	case c.stack[len(c.stack)-1].object:
		return stObjectNext
	default:
		return stArrayNext
	}
}

func (c *checker) afterValue() { c.state = c.valueEnded() }

func (c *checker) pop() {
	top := c.stack[len(c.stack)-1]
	if top.object {
		c.names = c.names[:top.namesFrom]
	}
	c.stack[len(c.stack)-1] = frame{}
	c.stack = c.stack[:len(c.stack)-1]
	c.afterValue()
}

func (c *checker) endString() error {
	if !c.key {
		c.afterValue()
		return nil
	}
	c.flushHigh()
	c.key = false
	top := &c.stack[len(c.stack)-1]
	name := string(c.keyBuf)
	if err := c.addName(top, name); err != nil {
		return err
	}
	top.name, top.hasName = name, true
	c.state = stColon
	return nil
}

// addName records a name of the innermost object, which is the only one that
// can be taking names: its names are the last in c.names.
func (c *checker) addName(f *frame, name string) error {
	if f.set != nil {
		if _, seen := f.set[name]; seen {
			return c.duplicate(name)
		}
		f.set[name] = struct{}{}
		return nil
	}
	for _, seen := range c.names[f.namesFrom:] {
		if seen == name {
			return c.duplicate(name)
		}
	}
	c.names = append(c.names, name)
	if len(c.names)-f.namesFrom > linearNames {
		f.set = make(map[string]struct{}, 2*linearNames)
		for _, seen := range c.names[f.namesFrom:] {
			f.set[seen] = struct{}{}
		}
		c.names = c.names[:f.namesFrom]
	}
	return nil
}

// keyRune adds the code point of a \uXXXX escape to the name being read. A
// surrogate pair is one code point, as in the name written out; a lone
// surrogate stays itself (see appendSurrogate), so that two names that differ
// only in one are two names, as I-JSON compares them.
func (c *checker) keyRune(r rune) {
	if c.high != 0 {
		if r >= 0xDC00 && r < 0xE000 {
			c.keyBuf = utf8.AppendRune(c.keyBuf, utf16.DecodeRune(c.high, r))
			c.high = 0
			return
		}
		c.flushHigh()
	}
	switch {
	case r >= 0xD800 && r < 0xDC00:
		c.high = r
	case r >= 0xDC00 && r < 0xE000:
		c.keyBuf = appendSurrogate(c.keyBuf, r)
	default:
		c.keyBuf = utf8.AppendRune(c.keyBuf, r)
	}
}

func (c *checker) flushHigh() {
	if c.high != 0 {
		c.keyBuf = appendSurrogate(c.keyBuf, c.high)
		c.high = 0
	}
}

// appendSurrogate writes a lone surrogate the way WTF-8 does: as the three
// bytes UTF-8 would give it, which UTF-8 itself never uses. (utf8.AppendRune
// would write U+FFFD, making every lone surrogate the same name.)
func appendSurrogate(b []byte, r rune) []byte {
	return append(b, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
}

func (c *checker) finish() error {
	switch c.state {
	case stDone:
		return nil
	case stNumZero, stNumInt, stNumFrac, stNumExpDigits:
		if len(c.stack) == 0 {
			return nil
		}
	case stValue:
		if c.offset == 0 || len(c.stack) == 0 {
			return &Error{Offset: c.offset, Path: "$", Reason: "no JSON value"}
		}
	}
	return &Error{Offset: c.offset, Path: c.path(), Reason: "the body ends inside the JSON value"}
}

func (c *checker) fail(i int, reason string) error {
	return &Error{Offset: c.offset + int64(i), Path: c.path(), Reason: reason}
}

func (c *checker) duplicate(name string) error {
	return &Error{Offset: c.keyAt, Path: c.path(), Duplicate: true, Name: name}
}

// path is the JSONPath of the value being read: the member being read in
// each object and the element in each array. While a name is read, the
// innermost object has no member yet, which is the path a repeated name
// reports.
func (c *checker) path() string {
	var b strings.Builder
	b.WriteByte('$')
	for _, f := range c.stack {
		switch {
		case !f.object:
			fmt.Fprintf(&b, "[%d]", f.index)
		case f.hasName:
			writePathName(&b, f.name)
		}
	}
	return b.String()
}

func unescaped(b byte) byte {
	switch b {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	}
	return b // '"', '\\' and '/' stand for themselves
}

func hexValue(b byte) (rune, bool) {
	switch {
	case b >= '0' && b <= '9':
		return rune(b - '0'), true
	case b >= 'a' && b <= 'f':
		return rune(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return rune(b-'A') + 10, true
	}
	return 0, false
}

func describe(b byte) string {
	if b >= 0x20 && b < 0x7f {
		return fmt.Sprintf("%q", rune(b))
	}
	return fmt.Sprintf("0x%02X", b)
}

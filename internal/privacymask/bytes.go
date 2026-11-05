package privacymask

import "regexp"

// The two helpers below live in magpie's model.go, which is not ported (see
// doc.go); the finders ported from its other files use them.

// uuidRe is a UUID in any case.
var uuidRe = regexp.MustCompile(`^(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// wordByte is a byte of an identifier: a letter, a digit or _.
func wordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

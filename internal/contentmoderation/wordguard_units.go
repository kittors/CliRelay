package contentmoderation

import "github.com/tidwall/gjson"

// unitKind says what a wordUnit is.
type unitKind uint8

const (
	// unitText is the text of one JSON string value.
	unitText unitKind = iota
	// unitOpaque is text that is only ever matched: an object key, or a deep
	// subtree read as raw JSON. A hit there cannot be replaced without changing
	// the structure of the document.
	unitOpaque
)

// wordUnit is one piece of request text the word guard can check and, when
// the place it sits in is known, rewrite.
//
// Units are the unit of matching: a rule never matches across two of them.
// That differs from the Profile keyword check, which joins all the text with
// spaces. The difference is deliberate. A replacement has to be written into
// one JSON string, and a word is not split across the strings of a request in
// any way the model reads as one word.
type wordUnit struct {
	class Class
	// user marks primary text of a user turn; see Segment.User.
	user bool
	// turn numbers the message the text came from; see Segment.Turn.
	turn int
	// text is the string's text as the client wrote it. It is not
	// whitespace-collapsed: offsets into it must stay offsets into the string.
	text string
	// lit0 and lit1 bound the string literal, quotes included, in the payload.
	lit0, lit1 int
	// rewritable says the literal at lit0:lit1 is known and holds exactly text.
	rewritable bool
}

// recordUnit remembers a piece of text and where it came from. at is the gjson
// value the text was read from.
func (x *extraction) recordUnit(class Class, user bool, text string, at gjson.Result, kind unitKind) {
	collapsed := collapseSpaces(text)
	if collapsed == "" {
		return
	}
	class, user = x.classify(class, user, collapsed)
	unit := wordUnit{class: class, user: user, turn: x.turn, text: text}
	if kind == unitText && x.nested == 0 && at.Type == gjson.String && at.Index > 0 {
		// The index gjson reports is trusted only after looking at the bytes it
		// points at: a splice at a wrong offset would corrupt the request.
		lit0, lit1 := at.Index, at.Index+len(at.Raw)
		if lit1 <= len(x.payload) && string(x.payload[lit0:lit1]) == at.Raw {
			unit.lit0, unit.lit1, unit.rewritable = lit0, lit1, true
		}
	}
	x.units = append(x.units, unit)
}

// newestUserTurn returns the turn of the newest user message. A request
// without any user text falls back to its newest primary message, as the
// moderation backends do (see splitNewestUserMessage).
func newestUserTurn(units []wordUnit) (turn int, found bool) {
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].class == ClassPrimary && units[i].user {
			return units[i].turn, true
		}
	}
	for i := len(units) - 1; i >= 0; i-- {
		if units[i].class == ClassPrimary {
			return units[i].turn, true
		}
	}
	return 0, false
}

// unitsInScope returns the indexes of the units a scan scope covers.
func unitsInScope(units []wordUnit, scope string) []int {
	newest, found := 0, false
	if scope == WordScanLastUser {
		newest, found = newestUserTurn(units)
	}
	in := make([]int, 0, len(units))
	for i, unit := range units {
		covered := true
		switch scope {
		case WordScanLastUser:
			covered = found && unit.class == ClassPrimary && unit.turn == newest
		case WordScanAllUser:
			covered = unit.class == ClassPrimary && unit.user
		case WordScanWithSystem:
			covered = unit.class == ClassPrimary
		}
		if covered {
			in = append(in, i)
		}
	}
	return in
}

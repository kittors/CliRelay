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
	// class and user classify the text as the moderation backends see it; they
	// are what a match reports as its source.
	class Class
	// user marks primary text of a user turn; see Segment.User.
	user bool
	// scopeClass and scopeUser are the role of the message the text came from,
	// before the extractor reclassified it. The scan scopes are judged by these:
	// a Claude Code <system-reminder> block is moved out of the user's own text
	// for the backends (class says tool output), but it is still a block of a
	// user message, and a caller who starts a text with that tag must not be able
	// to take it out of a scope that covers the user's messages.
	scopeClass Class
	scopeUser  bool
	// reminder says the extractor reclassified the text (it is a
	// <system-reminder> block).
	reminder bool
	// signed says the text is covered by a signature the upstream checks (a
	// replayed thinking block): changing a character of it makes the upstream
	// refuse the request, so it is matched but never rewritten.
	signed bool
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
	scopeClass, scopeUser := class, user
	class, user = x.classify(class, user, collapsed)
	unit := wordUnit{
		class: class, user: user, turn: x.turn, text: text,
		scopeClass: scopeClass, scopeUser: scopeUser,
		reminder: class != scopeClass || user != scopeUser,
		signed:   x.signed,
	}
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
//
// A <system-reminder> block does not make its message the newest one: Claude
// Code attaches them to messages of its own, after the one the person typed.
// They only decide when there is nothing else, so a request made of nothing but
// such blocks still has a newest message to check.
func newestUserTurn(units []wordUnit) (turn int, found bool) {
	type pick func(wordUnit) bool
	for _, accept := range []pick{
		func(u wordUnit) bool { return !u.reminder && u.scopeClass == ClassPrimary && u.scopeUser },
		func(u wordUnit) bool { return !u.reminder && u.scopeClass == ClassPrimary },
		func(u wordUnit) bool { return u.scopeClass == ClassPrimary && u.scopeUser },
		func(u wordUnit) bool { return u.scopeClass == ClassPrimary },
	} {
		for i := len(units) - 1; i >= 0; i-- {
			if accept(units[i]) {
				return units[i].turn, true
			}
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
			// The newest message, and any reminder the caller put after it: such a
			// block is not the newest message, but it is not for hiding text from
			// a check of the newest one either.
			covered = found && unit.scopeClass == ClassPrimary &&
				(unit.turn == newest || (unit.reminder && unit.scopeUser && unit.turn > newest))
		case WordScanAllUser:
			covered = unit.scopeClass == ClassPrimary && unit.scopeUser
		case WordScanWithSystem:
			covered = unit.scopeClass == ClassPrimary
		}
		if covered {
			in = append(in, i)
		}
	}
	return in
}

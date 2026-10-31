package contentmoderation

import "github.com/tidwall/gjson"

// unitKind says what a wordUnit is.
type unitKind uint8

const (
	// unitText is the text of one JSON string value.
	unitText unitKind = iota
	// unitKey is an object key. Renaming it changes the document's structure only
	// as far as the name goes, so it can be rewritten like a value as long as the
	// object keeps its keys apart (see renameKeys).
	unitKey
	// unitOpaque is text that is only ever matched: a subtree nested so deeply
	// that it is read as raw JSON. A hit there cannot be replaced without changing
	// the structure of the document.
	unitOpaque
)

// wordDoc is a JSON document that units sit in: the request itself, or the
// arguments of a tool call, which arrive as JSON text inside one of the
// request's strings. Text in such a document cannot be spliced into the request
// directly, because its offsets are the decoded string's, not the request's; it
// is rewritten in the document and the document is then written back into the
// string it came from.
type wordDoc struct {
	data []byte
	// parent is the document this one is the content of a string of; nil for the
	// request. lit0 and lit1 bound that string literal, quotes included, in
	// parent.data.
	parent     *wordDoc
	lit0, lit1 int
	depth      int
	// located says the literal is known, in this document and in every one above
	// it, and decodes to exactly data.
	located bool
}

// path says where the document sits in the request, for messages: the path of
// the string it is the content of, or "" for the request itself.
func (d *wordDoc) path() string {
	if d == nil || d.parent == nil {
		return ""
	}
	return joinJSONPath(d.parent.path(), jsonPathAt(d.parent.data, d.lit0))
}

// nestedDoc is the document inside the string value (the JSON text raw) of
// outer. It is located only if the string's place in outer is known.
func nestedDoc(outer *wordDoc, value gjson.Result, raw string) *wordDoc {
	doc := &wordDoc{data: []byte(raw), parent: outer, depth: outer.depth + 1}
	if !outer.located || value.Type != gjson.String || value.Index <= 0 {
		return doc
	}
	lit0, lit1 := value.Index, value.Index+len(value.Raw)
	if lit1 > len(outer.data) || string(outer.data[lit0:lit1]) != value.Raw {
		return doc
	}
	// The string must decode to exactly the document that was read from it, or
	// offsets in one mean nothing in the other.
	if text, ok := decodeJSONLiteral(outer.data[lit0:lit1]); !ok || text != raw {
		return doc
	}
	doc.lit0, doc.lit1, doc.located = lit0, lit1, true
	return doc
}

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
	kind unitKind
	// doc is the document the text sits in, and lit0 and lit1 bound the string
	// literal that holds it there, quotes included.
	doc        *wordDoc
	lit0, lit1 int
	// rewritable says the literal at lit0:lit1 of doc is known and holds exactly
	// text. If not, why says what stops it.
	rewritable bool
	why        string
	// owner0 and owner1 bound, for a key, the object it belongs to in doc.
	owner0, owner1 int
}

// What stops a unit from being rewritten, in words a caller can act on.
const (
	whyTooDeep   = "it is nested too deeply to be rewritten"
	whyUnlocated = "its place in the request could not be determined"
	whyInvalid   = "it is not valid UTF-8"
)

// recordUnit remembers a piece of text and where it came from. at is the gjson
// value the text was read from, and for a key owner is the object it belongs to.
func (x *extraction) recordUnit(class Class, user bool, text string, at, owner gjson.Result, kind unitKind) {
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
		kind:     kind,
		doc:      x.doc,
	}
	switch {
	case kind == unitOpaque:
		unit.why = whyTooDeep
		if x.doc != nil && at.Index > 0 {
			// Not rewritable, but its place is what a message names.
			unit.lit0, unit.lit1 = at.Index, at.Index+len(at.Raw)
		}
	case x.doc == nil || !x.doc.located || at.Type != gjson.String || at.Index <= 0:
		unit.why = whyUnlocated
	default:
		// The index gjson reports is trusted only after looking at the bytes it
		// points at: a splice at a wrong offset would corrupt the request.
		lit0, lit1 := at.Index, at.Index+len(at.Raw)
		if lit1 > len(x.doc.data) || string(x.doc.data[lit0:lit1]) != at.Raw {
			unit.why = whyUnlocated
			break
		}
		unit.lit0, unit.lit1, unit.rewritable = lit0, lit1, true
		if kind == unitKey {
			unit.owner0, unit.owner1 = owner.Index, owner.Index+len(owner.Raw)
			// The object is read from the same bytes to find the key's siblings, so
			// it is checked the same way.
			if owner.Raw == "" || owner.Index < 0 || unit.owner1 > len(x.doc.data) || string(x.doc.data[unit.owner0:unit.owner1]) != owner.Raw {
				unit.rewritable, unit.why = false, whyUnlocated
			}
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

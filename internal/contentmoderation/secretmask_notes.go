package contentmoderation

import (
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
)

// maskNotes collects, while the first copy of a request is masked, what its
// record shows besides the values: where each value was first found, and the
// text around its first place as the upstream got it (Masker.View: the
// placeholders in, a value sent as written shown by its kind).
//
// Where a value was found is noted as the unit it was found in, and made a
// field once the whole request is read (locate), for all of them in one walk
// of the request: a walk per unit is a walk of megabytes per string on a
// request that carries screenshots.
type maskNotes struct {
	fields   map[string]string // value id → field
	snippets []MaskSnippet
	// Until locate: the units values were first found in, which of them each
	// value's field is (placeOf), and each snippet's (pending).
	places  []wordUnit
	placeOf map[string]int
	pending []pendingSnippet
}

// pendingSnippet is a snippet whose field is not known yet: snippets[snippet]
// shows a value in places[place].
type pendingSnippet struct{ snippet, place int }

func newMaskNotes() *maskNotes { return &maskNotes{fields: map[string]string{}} }

// locate makes the fields of the values noted so far, and lets go of the units
// they were found in: those hold the request.
func (n *maskNotes) locate() {
	if len(n.places) == 0 {
		return
	}
	fields := unitFields(n.places)
	for i, field := range fields {
		fields[i] = withoutSecrets(field)
	}
	for id, place := range n.placeOf {
		n.fields[id] = fields[place]
	}
	for _, p := range n.pending {
		n.snippets[p.snippet].Field = fields[p.place]
	}
	n.places, n.placeOf, n.pending = nil, nil, nil
}

const (
	// maskSnippetRadius is how many characters a snippet shows on each side of
	// a value.
	maskSnippetRadius = 120
	// maxMaskSnippets bounds the snippets of one record.
	maxMaskSnippets = 8
)

// add notes text unit i of the request, which the masker planned edits for.
func (n *maskNotes) add(m *privacymask.Masker, i int, unit wordUnit, edits []privacymask.Edit) {
	view, marks := m.View(i, unit.text, edits)
	if len(marks) == 0 {
		return
	}
	place := -1 // of unit in places, once a value is first found in it
	note := func(id string) {
		if place < 0 {
			place = len(n.places)
			n.places = append(n.places, unit)
		}
		if n.placeOf == nil {
			n.placeOf = make(map[string]int)
		}
		n.fields[id] = "" // located later; present now, as the first place is the one shown
		n.placeOf[id] = place
	}
	for j := 0; j < len(marks); j++ {
		mark := marks[j]
		if _, seen := n.fields[mark.ID]; seen {
			continue // a snippet shows a value's first place only
		}
		note(mark.ID)
		if len(n.snippets) >= maxMaskSnippets {
			continue
		}
		lo := runesBefore(view, mark.Start, maskSnippetRadius)
		for k := j - 1; k >= 0 && marks[k].End > lo; k-- {
			lo = min(lo, marks[k].Start) // never half a placeholder
		}
		hi := runesAfter(view, mark.End, maskSnippetRadius)
		for j+1 < len(marks) && marks[j+1].Start < hi {
			j++
			if _, seen := n.fields[marks[j].ID]; !seen {
				note(marks[j].ID)
			}
			hi = max(hi, marks[j].End)
		}
		text := view[lo:hi]
		if lo > 0 {
			text = "…" + text
		}
		if hi < len(view) {
			text += "…"
		}
		n.pending = append(n.pending, pendingSnippet{snippet: len(n.snippets), place: place})
		n.snippets = append(n.snippets, MaskSnippet{Role: unitRole(unit), Text: text})
	}
}

// runesBefore is the byte offset n characters before at in s, or 0.
func runesBefore(s string, at, n int) int {
	for ; n > 0 && at > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:at])
		at -= size
	}
	return at
}

// runesAfter is the byte offset n characters after at in s, or len(s).
func runesAfter(s string, at, n int) int {
	for ; n > 0 && at < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[at:])
		at += size
	}
	return at
}

// unitRole is who wrote the text, as a record shows it.
func unitRole(u wordUnit) string {
	switch {
	case u.class == ClassToolOutput:
		return "tool"
	case u.scopeUser:
		return "user"
	case u.scopeClass == ClassAssistantHistory:
		return "assistant"
	}
	return "system"
}

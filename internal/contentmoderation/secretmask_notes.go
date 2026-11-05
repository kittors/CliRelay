package contentmoderation

import (
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
)

// maskNotes collects, while the first copy of a request is masked, what its
// record shows besides the values: where each value was first found, and the
// text around its first place as the upstream got it (Masker.View: the
// placeholders in, a value sent as written shown by its kind).
type maskNotes struct {
	fields   map[string]string // value id → field
	snippets []MaskSnippet
}

func newMaskNotes() *maskNotes { return &maskNotes{fields: map[string]string{}} }

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
	field := ""
	for j := 0; j < len(marks); j++ {
		mark := marks[j]
		if _, seen := n.fields[mark.ID]; seen {
			continue // a snippet shows a value's first place only
		}
		if field == "" {
			field = withoutSecrets(unitField(unit))
		}
		n.fields[mark.ID] = field
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
				n.fields[marks[j].ID] = field
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
		n.snippets = append(n.snippets, MaskSnippet{Field: field, Role: unitRole(unit), Text: text})
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

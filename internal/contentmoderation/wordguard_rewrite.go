package contentmoderation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// replacement is a span of a unit's text to be replaced.
type replacement struct {
	start, end int
	text       string
	// order ranks contexts, so that where spans overlap the platform's
	// replacement text wins.
	order int
}

// replaceTarget is what one replace context found in one unit.
type replaceTarget struct {
	context int
	// hits are byte ranges of the unit's normalized text.
	hits []wordHit
}

// rewriteFailure says why a request could not be rewritten and where.
type rewriteFailure struct {
	// reason is WordReasonUnrewritable or WordReasonRewriteInvalid.
	reason string
	// field is the path of the text that stopped it, and why what stopped it.
	field, why string
}

// What else can stop a rewrite, besides the reasons a unit carries.
const (
	whyEscapes     = "the escape sequences of its string could not be read back"
	whyInvalidJSON = "rewriting it would leave invalid JSON"
	whyOverlap     = "two of the replacements fall in the same place"
)

// unrewritableMessage tells the client what could not be replaced and where, on
// top of the policy's block message. The path is the client's own: it is read
// from the request it sent, and says nothing of the rules.
func unrewritableMessage(base string, failure *rewriteFailure) string {
	if failure == nil || failure.field == "" {
		return base
	}
	field := failure.field
	if len(field) > 200 {
		field = field[:200] + "..."
	}
	return fmt.Sprintf("%s The policy replaces banned words, but the text in %s cannot be replaced (%s); remove or reword it.", base, field, failure.why)
}

// unitField is the path of the text of a unit in the request, with a note when
// it is an object key.
func unitField(unit wordUnit) string {
	if unit.doc == nil {
		return ""
	}
	field := joinJSONPath(unit.doc.path(), jsonPathAt(unit.doc.data, unit.lit0))
	if field != "" && unit.kind == unitKey {
		field += " (an object key)"
	}
	return field
}

// patch replaces the string literal at lit0:lit1 of a document.
type patch struct {
	lit0, lit1 int
	literal    []byte
}

// keyGroup is the object a set of renamed keys belongs to.
type keyGroup struct {
	doc            *wordDoc
	owner0, owner1 int
}

type keyRename struct {
	lit0, lit1 int
	text       string
	edits      []literalEdit
}

// rewriteWordUnits applies the replace action: for every unit that has a hit,
// it maps the hits back onto the text as written and splices the replacement
// into the unit's string literal. The result is the whole request body, byte
// for byte as it was outside those literals.
//
// Text inside the JSON of a tool call's arguments is rewritten in that document
// (see wordDoc) and the document's changes are then spliced into the string it
// sits in, so the rest of that string keeps its exact bytes too.
//
// A failure means the request could not be rewritten safely.
func rewriteWordUnits(units []wordUnit, contexts []wordContext, wanted map[int][]replaceTarget, normalizedText func(int, bool) string) ([]byte, int, *rewriteFailure) {
	fail := func(ui int, reason, why string) *rewriteFailure {
		return &rewriteFailure{reason: reason, field: unitField(units[ui]), why: why}
	}

	// Units that share a literal (the same string reached twice) are rewritten
	// once, with the hits of all of them.
	type slot struct {
		doc  *wordDoc
		lit0 int
	}
	bySlot := make(map[slot][]int)
	var slots []slot
	var root *wordDoc
	for ui := range units {
		if len(wanted[ui]) == 0 {
			continue
		}
		unit := units[ui]
		if !unit.rewritable {
			return nil, 0, fail(ui, WordReasonUnrewritable, unit.why)
		}
		if root == nil {
			root = unit.doc
			for root.parent != nil {
				root = root.parent
			}
		}
		key := slot{unit.doc, unit.lit0}
		if _, seen := bySlot[key]; !seen {
			slots = append(slots, key)
		}
		bySlot[key] = append(bySlot[key], ui)
	}
	if root == nil {
		return nil, 0, &rewriteFailure{reason: WordReasonUnrewritable, why: whyUnlocated}
	}

	var patches docPatches

	replaced := 0
	var groups []keyGroup
	renames := make(map[keyGroup][]keyRename)
	for _, sl := range slots {
		first := bySlot[sl][0]
		unit := units[first]
		lit := sl.doc.data[unit.lit0:unit.lit1]
		// The extractor read this text through gjson; the splice reads the raw
		// bytes. They must agree on what the string says, or offsets would not.
		if text, ok := decodeJSONLiteral(lit); !ok || text != unit.text {
			return nil, 0, fail(first, WordReasonUnrewritable, whyUnlocated)
		}
		var spans []replacement
		for _, ui := range bySlot[sl] {
			found, why := replacementsForUnit(units[ui], ui, wanted[ui], contexts, normalizedText)
			if why != "" {
				return nil, 0, fail(ui, WordReasonUnrewritable, why)
			}
			spans = append(spans, found...)
		}
		edits := mergeReplacements(spans)
		if len(edits) == 0 {
			return nil, 0, fail(first, WordReasonUnrewritable, whyUnlocated)
		}
		replaced += len(edits)
		if unit.kind == unitKey {
			group := keyGroup{doc: sl.doc, owner0: unit.owner0, owner1: unit.owner1}
			if _, seen := renames[group]; !seen {
				groups = append(groups, group)
			}
			renames[group] = append(renames[group], keyRename{lit0: unit.lit0, lit1: unit.lit1, text: unit.text, edits: edits})
			continue
		}
		literal, ok := spliceLiteral(lit, edits)
		if !ok {
			return nil, 0, fail(first, WordReasonUnrewritable, whyEscapes)
		}
		patches.add(sl.doc, patch{lit0: unit.lit0, lit1: unit.lit1, literal: literal})
	}
	for _, group := range groups {
		for _, p := range renameKeys(group, renames[group]) {
			patches.add(group.doc, p)
		}
	}

	out, failure := patches.write()
	if failure != nil {
		return nil, 0, failure
	}
	return out, replaced, nil
}

// docPatches collects the literal patches of a rewrite by document and writes
// them back into the request.
type docPatches struct {
	patches map[*wordDoc][]patch
	byDepth map[int][]*wordDoc
	deepest int
}

func (d *docPatches) add(doc *wordDoc, p patch) {
	if d.patches == nil {
		d.patches = make(map[*wordDoc][]patch)
		d.byDepth = make(map[int][]*wordDoc)
	}
	if len(d.patches[doc]) == 0 {
		d.byDepth[doc.depth] = append(d.byDepth[doc.depth], doc)
		d.deepest = max(d.deepest, doc.depth)
	}
	d.patches[doc] = append(d.patches[doc], p)
}

// write returns the request with the patches applied.
func (d *docPatches) write() ([]byte, *rewriteFailure) {
	// Innermost documents first: one that changed is written back into the string
	// it is the content of, which is a change to the document above it.
	var out []byte
	for depth := d.deepest; depth >= 0; depth-- {
		for _, doc := range d.byDepth[depth] {
			ps := d.patches[doc]
			sort.SliceStable(ps, func(i, j int) bool { return ps[i].lit0 < ps[j].lit0 })
			changed := make([]byte, 0, len(doc.data)+64)
			cursor := 0
			for _, p := range ps {
				if p.lit0 < cursor {
					return nil, &rewriteFailure{reason: WordReasonRewriteInvalid, field: doc.path(), why: whyOverlap}
				}
				changed = append(changed, doc.data[cursor:p.lit0]...)
				changed = append(changed, p.literal...)
				cursor = p.lit1
			}
			changed = append(changed, doc.data[cursor:]...)
			if !gjson.ValidBytes(changed) {
				return nil, &rewriteFailure{reason: WordReasonRewriteInvalid, field: doc.path(), why: whyInvalidJSON}
			}
			if doc.parent == nil {
				out = changed
				continue
			}
			edits := make([]literalEdit, len(ps))
			for i, p := range ps {
				edits[i] = literalEdit{start: p.lit0, end: p.lit1, text: string(p.literal)}
			}
			literal, ok := spliceLiteral(doc.parent.data[doc.lit0:doc.lit1], edits)
			if !ok {
				return nil, &rewriteFailure{reason: WordReasonUnrewritable, field: doc.path(), why: whyEscapes}
			}
			d.add(doc.parent, patch{lit0: doc.lit0, lit1: doc.lit1, literal: literal})
		}
	}
	if out == nil {
		return nil, &rewriteFailure{reason: WordReasonRewriteInvalid, why: whyInvalidJSON}
	}
	return out, nil
}

// renameKeys writes the new names of the keys of one object. Two keys that
// come out the same would leave one of their values unreachable (a JSON parser
// keeps the last, or refuses the document), so a name that is taken gets a
// number: ***, ***_2, ***_3.
func renameKeys(group keyGroup, jobs []keyRename) []patch {
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].lit0 < jobs[j].lit0 })
	renaming := make(map[int]bool, len(jobs))
	for _, job := range jobs {
		renaming[job.lit0] = true
	}
	taken := make(map[string]bool)
	gjson.ParseBytes(group.doc.data[group.owner0:group.owner1]).ForEach(func(key, _ gjson.Result) bool {
		if !renaming[group.owner0+key.Index] {
			taken[key.String()] = true
		}
		return true
	})
	patches := make([]patch, 0, len(jobs))
	for _, job := range jobs {
		name := applyLiteralEdits(job.text, job.edits)
		final := name
		for n := 2; taken[final]; n++ {
			final = name + "_" + strconv.Itoa(n)
		}
		taken[final] = true
		literal := append([]byte{'"'}, appendJSONEscaped(nil, final)...)
		patches = append(patches, patch{lit0: job.lit0, lit1: job.lit1, literal: append(literal, '"')})
	}
	return patches
}

// applyLiteralEdits applies edits (ascending, not overlapping) to text.
func applyLiteralEdits(text string, edits []literalEdit) string {
	var b strings.Builder
	cursor := 0
	for _, edit := range edits {
		b.WriteString(text[cursor:edit.start])
		b.WriteString(edit.text)
		cursor = edit.end
	}
	b.WriteString(text[cursor:])
	return b.String()
}

// replacementsForUnit maps the hits of the replace contexts that hit the unit
// back onto the text as written. The string it returns says what stopped it.
func replacementsForUnit(unit wordUnit, ui int, targets []replaceTarget, contexts []wordContext, normalizedText func(int, bool) string) ([]replacement, string) {
	var out []replacement
	mappedByFold := make(map[bool]foldedText)
	for _, target := range targets {
		context := contexts[target.context]
		mapped, ok := mappedByFold[context.fold]
		if !ok {
			mapped = normalizeMapped(unit.text, context.fold)
			mappedByFold[context.fold] = mapped
		}
		// The hits are offsets into the normalized text. The mapped text is the
		// same string by construction; if it ever is not, the offsets mean
		// nothing in it and the request is not rewritten on a guess. A text that
		// is not valid UTF-8 cannot be traced back at all.
		if !mapped.exact {
			return nil, whyInvalid
		}
		if mapped.text != normalizedText(ui, context.fold) {
			return nil, whyUnlocated
		}
		spans := make([]textSpan, 0, len(target.hits))
		for _, hit := range target.hits {
			spans = append(spans, textSpan{start: hit.start, end: hit.end})
		}
		original, ok := mapped.originalSpans(spans)
		if !ok {
			return nil, whyUnlocated
		}
		for _, span := range original {
			out = append(out, replacement{start: span.start, end: span.end, text: context.replacement, order: target.context})
		}
	}
	return out, ""
}

// mergeReplacements orders spans and joins the ones that overlap or touch the
// same character. Where they overlap, the replacement of the span that begins
// first (the platform's, on a tie) is used.
func mergeReplacements(spans []replacement) []literalEdit {
	if len(spans) == 0 {
		return nil
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		if spans[i].order != spans[j].order {
			return spans[i].order < spans[j].order
		}
		return spans[i].end > spans[j].end
	})
	edits := []literalEdit{{start: spans[0].start, end: spans[0].end, text: spans[0].text}}
	for _, span := range spans[1:] {
		last := &edits[len(edits)-1]
		if span.start < last.end {
			if span.end > last.end {
				last.end = span.end
			}
			continue
		}
		edits = append(edits, literalEdit{start: span.start, end: span.end, text: span.text})
	}
	return edits
}

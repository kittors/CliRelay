package contentmoderation

import (
	"strings"
	"unicode/utf8"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// maxReplyHold bounds how much text one stream position keeps back while it
// waits to see whether a match continues. Keywords are short, so it is only
// reached by an expression that can match without end (a run of digits, say):
// there the match is cut at the bound and the text so far is decided.
const maxReplyHold = 4096

// replyEngine applies the word guard to what the model says. It is shared by
// the non-streaming and the streaming filter of one request.
//
// A reply is filtered with the contexts that switched reply filtering on. Their
// action decides what a hit does to the reply, which differs a little from the
// request side: the reply is already on its way to the client, so there is no
// one to refuse. A context set to block therefore replaces, with its replacement
// text, exactly as one set to replace does; a context set to log counts and
// leaves the text alone.
type replyEngine struct {
	contexts []wordContext
	snapshot *WordGuardSnapshot
	// budget limits the regular expressions' work over the whole reply, however
	// many pieces it arrives in. A reply is not bounded the way a request is by
	// its body, so this is the bound that keeps a long answer from costing more
	// than a request may.
	budget *regexBudget
}

// newReplyEngine returns the engine for the contexts that filter replies, or
// nil if none does.
func newReplyEngine(contexts []wordContext, snapshot *WordGuardSnapshot) *replyEngine {
	var chosen []wordContext
	for _, context := range contexts {
		if context.reply && context.matcher != nil {
			chosen = append(chosen, context)
		}
	}
	if len(chosen) == 0 {
		return nil
	}
	return &replyEngine{contexts: chosen, snapshot: snapshot, budget: newRegexBudget()}
}

func (e *replyEngine) pieces(format sdktranslator.Format, doc []byte, stream bool) ([]replyPiece, int) {
	return replyPieces(format, doc, stream)
}

func (e *replyEngine) skipReply(reason string) { e.snapshot.skipReply(reason) }

// replyText is the outcome of filtering one text.
type replyText struct {
	// out is the text to send now.
	out string
	// held is the end of the input, as written, that must wait for the text that
	// follows it. It is empty unless the caller said more text may come.
	held string
}

// filterText replaces the words in text. With more set, the text is a piece of
// a longer one and its end is not final: a match that could still grow, or that
// could still begin, at the end is left in held, to be filtered again together
// with what follows (the caller puts held in front of the next piece of the
// same text). Without it the end is final and held is empty.
func (e *replyEngine) filterText(text string, more bool) replyText {
	if text == "" {
		return replyText{}
	}
	var spans []replacement
	cut := len(text)
	logged := 0
	skipped := ""
	for ci, context := range e.contexts {
		rewrites := context.action != WordActionLog
		options := findOptions{more: more && rewrites, budget: e.budget}
		if !rewrites {
			options.limit = maxLoggedHitsPerUnit
		}
		// Most text has nothing in it, so the cheap normalization decides that
		// first; only text with a hit, or the start of one, is traced back.
		normalized := normalizeForMatch(text, context.fold)
		found := context.matcher.find(normalized, options)
		if len(found.hits) == 0 && found.undecided < 0 {
			continue
		}
		if !rewrites {
			logged += len(found.hits)
			continue
		}
		mapped := normalizeMapped(text, context.fold)
		if !mapped.exact || mapped.text != normalized {
			// A text that is not valid UTF-8 cannot be traced back to what was
			// written, so it cannot be rewritten; and the two normalizations must
			// agree on the string the hits were found in.
			skipped = "untraceable_text"
			continue
		}
		if len(found.hits) > 0 {
			ranges := make([]textSpan, len(found.hits))
			for i, hit := range found.hits {
				ranges[i] = textSpan{start: hit.start, end: hit.end}
			}
			original, ok := mapped.originalSpans(ranges)
			if !ok {
				skipped = "untraceable_text"
				continue
			}
			for _, span := range original {
				spans = append(spans, replacement{start: span.start, end: span.end, text: context.replacement, order: ci})
			}
		}
		if found.undecided >= 0 {
			cut = min(cut, originalStart(mapped, found.undecided))
		}
	}
	edits := mergeReplacements(spans)
	if more {
		// A final match from one context may straddle the point another context
		// is undecided from. Holding back half of it would let the word through
		// in pieces, so the hold starts where that match starts.
		for moved := true; moved; {
			moved = false
			for _, edit := range edits {
				if edit.start < cut && edit.end > cut {
					cut, moved = edit.start, true
				}
			}
		}
		if len(text)-cut > maxReplyHold {
			return e.filterText(text, false)
		}
	}
	var out strings.Builder
	out.Grow(len(text))
	pos, applied := 0, 0
	for _, edit := range edits {
		if edit.start >= cut {
			break // found again, with its whole text, when the held part is filtered
		}
		out.WriteString(text[pos:edit.start])
		out.WriteString(edit.text)
		pos = edit.end
		applied++
	}
	out.WriteString(text[pos:cut])
	e.snapshot.addReply(applied+logged, applied)
	e.snapshot.skipReply(skipped)
	if e.budget.wasExceeded() {
		e.snapshot.skipReply(ReplySkipRegexBudget)
	}
	return replyText{out: out.String(), held: text[cut:]}
}

// originalStart is the offset in the original text where the character at
// offset at of the normalized text begins.
func originalStart(mapped foldedText, at int) int {
	_, size := utf8.DecodeRuneInString(mapped.text[at:])
	spans, ok := mapped.originalSpans([]textSpan{{start: at, end: at + size}})
	if !ok {
		return 0 // cannot tell where it began: hold everything, which is always safe
	}
	return spans[0].start
}

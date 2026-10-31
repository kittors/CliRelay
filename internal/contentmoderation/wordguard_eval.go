package contentmoderation

import (
	"sort"
	"time"
	"unicode/utf16"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// What the word guard did to a request, as the request log and the panel's test
// dialog report it. Block, replace and log are the three actions; allow means
// nothing matched.
const (
	WordVerdictAllow   = "allow"
	WordVerdictLog     = "log"
	WordVerdictReplace = "replace"
	WordVerdictBlock   = "block"
)

// Reasons a request is blocked other than a hit under the block action.
const (
	// WordReasonUnrewritable: a hit under the replace action sits where it
	// cannot be replaced (an object key, JSON nested in a string, text that is not
	// valid UTF-8). Letting it through would defeat the policy, so the request is
	// refused instead.
	WordReasonUnrewritable = "unrewritable"
	// WordReasonRewriteInvalid: the rewritten body failed its checks. It is
	// refused rather than sent on.
	WordReasonRewriteInvalid = "rewrite_invalid"
)

const (
	// maxRecordedWordMatches bounds the matches kept for the request log.
	maxRecordedWordMatches = 20
	// maxLoggedHitsPerUnit stops counting in a text that is nothing but hits
	// when the action is only to log them.
	maxLoggedHitsPerUnit = 100
)

// WordMatch describes one hit.
type WordMatch struct {
	// Layer is the policy layer of the rule that matched: platform, tenant or key.
	Layer   string `json:"layer"`
	RuleID  string `json:"rule_id"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	// Action is what the hit led to: block, replace or log.
	Action string `json:"action"`
	// Source is the class of the text that held it: primary, tool_output or
	// assistant_history.
	Source string `json:"source"`
	// Unit numbers the text in the request, in document order. Start and End
	// locate the hit in that text, in UTF-16 code units like a JavaScript
	// string index; NormalizedOnly says the text as written differs from what
	// matched (it was found only after case, width, zero-width characters and
	// whitespace were normalized). They are filled in only when asked for, as
	// the panel's test dialog does.
	Unit           int  `json:"unit"`
	Start          int  `json:"start"`
	End            int  `json:"end"`
	NormalizedOnly bool `json:"normalized_only"`
}

// WordVerdict is the outcome of checking one request.
type WordVerdict struct {
	// Action is allow, log, replace or block: the strongest thing any hit led to.
	Action  string `json:"action"`
	Blocked bool   `json:"blocked"`
	// BlockMessage is what the client is told when Blocked.
	BlockMessage string `json:"block_message,omitempty"`
	// Reason is set when the request is blocked for a reason other than a hit
	// under the block action.
	Reason string `json:"reason,omitempty"`
	// Matches holds the first few hits; MatchCount counts them all.
	Matches    []WordMatch `json:"matches"`
	MatchCount int         `json:"match_count"`
	// Replaced counts the spans that were replaced.
	Replaced int `json:"replaced"`
	// Payload is the request with the replacements applied, or nil when nothing
	// was replaced.
	Payload   []byte `json:"-"`
	LatencyMS int64  `json:"latency_ms"`
	// Field names the text a replace could not rewrite (Reason unrewritable): where
	// it sits in the request, never what it says.
	Field string `json:"field,omitempty"`
	// RegexSkipped says the regular expressions could not all be run: the request
	// is larger than the work they are allowed (see regexBudget), so some of its
	// text was checked by keywords only. The request went on.
	RegexSkipped bool `json:"regex_skipped,omitempty"`
}

type wordEvalOptions struct {
	// positions asks for the location of each recorded hit (see WordMatch).
	positions bool
	// budget limits the regular expressions' work. It is shared when one request
	// is evaluated more than once; nil gives the evaluation a budget of its own.
	budget *regexBudget
}

// wordUnitHit is a hit in the normalized text of one unit.
type wordUnitHit struct {
	context int
	unit    int
	hit     wordHit
}

// evaluateWords checks a request against the contexts and, under the replace
// action, rewrites it. payload is the request body in the layout of format.
func evaluateWords(format sdktranslator.Format, payload []byte, contexts []wordContext, options wordEvalOptions) WordVerdict {
	started := time.Now()
	verdict := evaluateWordsUnits(extractWordUnits(format, payload), payload, contexts, options)
	verdict.LatencyMS = time.Since(started).Milliseconds()
	return verdict
}

func evaluateWordsUnits(units []wordUnit, payload []byte, contexts []wordContext, options wordEvalOptions) WordVerdict {
	if options.budget == nil {
		options.budget = newRegexBudget()
	}
	verdict := scanWordUnits(units, payload, contexts, options)
	verdict.RegexSkipped = options.budget.wasExceeded()
	return verdict
}

func scanWordUnits(units []wordUnit, payload []byte, contexts []wordContext, options wordEvalOptions) WordVerdict {
	verdict := WordVerdict{Action: WordVerdictAllow, Matches: []WordMatch{}}
	if len(units) == 0 || len(contexts) == 0 {
		return verdict
	}
	normalized := make(map[normalizedKey]string)
	normalizedText := func(unit int, fold bool) string {
		key := normalizedKey{unit: unit, fold: fold}
		text, ok := normalized[key]
		if !ok {
			text = normalizeForMatch(units[unit].text, fold)
			normalized[key] = text
		}
		return text
	}

	var hits []wordUnitHit
	// replaceHits lists, per unit, the replace contexts that hit it and where.
	// Which contexts hit which text is decided here, with the whole request in
	// view (the "newest user message" scope cannot be judged from one text
	// alone). The rewrite uses these hits as they are: they were found in the
	// text that is mapped back to the original, so it does not search again.
	replaceHits := make(map[int][]replaceTarget)
	for ci, context := range contexts {
		if context.matcher == nil {
			continue
		}
		find := findOptions{budget: options.budget}
		switch context.action {
		case WordActionBlock:
			find.limit = 1
		case WordActionLog:
			find.limit = maxLoggedHitsPerUnit
		}
		for _, ui := range unitsInScope(units, context.scanScope) {
			text := normalizedText(ui, context.fold)
			if text == "" {
				continue
			}
			found := context.matcher.find(text, find).hits
			for _, hit := range found {
				hits = append(hits, wordUnitHit{context: ci, unit: ui, hit: hit})
			}
			if len(found) > 0 && context.action == WordActionReplace {
				replaceHits[ui] = append(replaceHits[ui], replaceTarget{context: ci, hits: found})
			}
			if len(found) > 0 && context.action == WordActionBlock {
				verdict.Action, verdict.Blocked, verdict.BlockMessage = WordVerdictBlock, true, context.blockMessage
				verdict.MatchCount = len(hits)
				verdict.Matches = describeWordHits(units, contexts, hits, normalizedText, options)
				return verdict
			}
		}
	}
	if len(hits) == 0 {
		return verdict
	}
	verdict.MatchCount = len(hits)
	verdict.Action = WordVerdictLog
	if len(replaceHits) > 0 {
		verdict.Action = WordVerdictReplace
	}
	if verdict.Action == WordVerdictReplace {
		rewritten, replaced, reason := rewriteWordUnits(units, payload, contexts, replaceHits, normalizedText)
		if reason != "" {
			verdict.Action, verdict.Blocked, verdict.Reason = WordVerdictBlock, true, reason
			verdict.BlockMessage = firstReplaceContext(contexts).blockMessage
		} else {
			verdict.Payload, verdict.Replaced = rewritten, replaced
		}
	}
	verdict.Matches = describeWordHits(units, contexts, hits, normalizedText, options)
	return verdict
}

type normalizedKey struct {
	unit int
	fold bool
}

func firstReplaceContext(contexts []wordContext) wordContext {
	for _, context := range contexts {
		if context.action == WordActionReplace {
			return context
		}
	}
	return contexts[0]
}

// describeWordHits turns the first hits into WordMatches.
func describeWordHits(units []wordUnit, contexts []wordContext, hits []wordUnitHit, normalizedText func(int, bool) string, options wordEvalOptions) []WordMatch {
	matches := make([]WordMatch, 0, min(len(hits), maxRecordedWordMatches))
	for _, h := range hits {
		if len(matches) == maxRecordedWordMatches {
			break
		}
		context := contexts[h.context]
		rule := context.matcher.rules[h.hit.rule]
		match := WordMatch{
			Layer:   rule.Layer,
			RuleID:  rule.ID,
			Kind:    rule.Kind,
			Pattern: rule.Pattern,
			Action:  context.action,
			Source:  units[h.unit].class.String(),
			Unit:    h.unit,
		}
		if options.positions {
			locateWordHit(&match, units[h.unit], context.fold, h.hit, normalizedText(h.unit, context.fold))
		}
		matches = append(matches, match)
	}
	return matches
}

// locateWordHit fills in where a hit is in the text as written.
func locateWordHit(match *WordMatch, unit wordUnit, fold bool, hit wordHit, normalizedText string) {
	mapped := normalizeMapped(unit.text, fold)
	if !mapped.exact {
		return
	}
	spans, ok := mapped.originalSpans([]textSpan{{start: hit.start, end: hit.end}})
	if !ok {
		return
	}
	span := spans[0]
	match.Start = utf16Offset(unit.text, span.start)
	match.End = utf16Offset(unit.text, span.end)
	match.NormalizedOnly = unit.text[span.start:span.end] != normalizedText[hit.start:hit.end]
}

// utf16Offset converts a byte offset in text to a count of UTF-16 code units.
func utf16Offset(text string, offset int) int {
	count := 0
	for _, r := range text[:offset] {
		if n := utf16.RuneLen(r); n > 0 {
			count += n
		} else {
			count++
		}
	}
	return count
}

// replacement is a span of a unit's text to be replaced.
type replacement struct {
	start, end int
	text       string
	// order ranks contexts, so that where spans overlap the platform's
	// replacement text wins.
	order int
}

// rewriteWordUnits applies the replace action: for every unit that has a hit,
// it maps the hits back onto the text as written and splices the replacement
// into the unit's string literal. The result is the whole request body, byte
// for byte as it was outside those literals. A non-empty reason means the
// request could not be rewritten safely.
func rewriteWordUnits(units []wordUnit, payload []byte, contexts []wordContext, wanted map[int][]replaceTarget, normalizedText func(int, bool) string) ([]byte, int, string) {
	type patch struct {
		lit0, lit1 int
		literal    []byte
	}
	// Units that share a literal (the same string reached twice) are rewritten
	// once, with the hits of all of them.
	byLiteral := make(map[int][]int)
	var literals []int
	for ui := range units {
		if len(wanted[ui]) == 0 {
			continue
		}
		unit := units[ui]
		if !unit.rewritable {
			return nil, 0, WordReasonUnrewritable
		}
		if _, seen := byLiteral[unit.lit0]; !seen {
			literals = append(literals, unit.lit0)
		}
		byLiteral[unit.lit0] = append(byLiteral[unit.lit0], ui)
	}
	sort.Ints(literals)
	patches := make([]patch, 0, len(literals))
	replaced := 0
	for _, lit0 := range literals {
		first := units[byLiteral[lit0][0]]
		lit := payload[first.lit0:first.lit1]
		// The extractor read this text through gjson; the splice reads the raw
		// bytes. They must agree on what the string says, or offsets would not.
		if text, ok := decodeJSONLiteral(lit); !ok || text != first.text {
			return nil, 0, WordReasonUnrewritable
		}
		var spans []replacement
		for _, ui := range byLiteral[lit0] {
			found, reason := replacementsForUnit(units[ui], ui, wanted[ui], contexts, normalizedText)
			if reason != "" {
				return nil, 0, reason
			}
			spans = append(spans, found...)
		}
		edits := mergeReplacements(spans)
		if len(edits) == 0 {
			return nil, 0, WordReasonUnrewritable
		}
		literal, ok := spliceLiteral(lit, edits)
		if !ok {
			return nil, 0, WordReasonUnrewritable
		}
		replaced += len(edits)
		patches = append(patches, patch{lit0: first.lit0, lit1: first.lit1, literal: literal})
	}
	out := make([]byte, 0, len(payload)+64)
	cursor := 0
	for _, p := range patches {
		if p.lit0 < cursor {
			return nil, 0, WordReasonRewriteInvalid
		}
		out = append(out, payload[cursor:p.lit0]...)
		out = append(out, p.literal...)
		cursor = p.lit1
	}
	out = append(out, payload[cursor:]...)
	if !gjson.ValidBytes(out) {
		return nil, 0, WordReasonRewriteInvalid
	}
	return out, replaced, ""
}

// replaceTarget is what one replace context found in one unit.
type replaceTarget struct {
	context int
	// hits are byte ranges of the unit's normalized text.
	hits []wordHit
}

// replacementsForUnit maps the hits of the replace contexts that hit the unit
// back onto the text as written.
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
		if !mapped.exact || mapped.text != normalizedText(ui, context.fold) {
			return nil, WordReasonUnrewritable
		}
		spans := make([]textSpan, 0, len(target.hits))
		for _, hit := range target.hits {
			spans = append(spans, textSpan{start: hit.start, end: hit.end})
		}
		original, ok := mapped.originalSpans(spans)
		if !ok {
			return nil, WordReasonUnrewritable
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

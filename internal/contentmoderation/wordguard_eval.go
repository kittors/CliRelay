package contentmoderation

import (
	"time"
	"unicode/utf16"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
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
	Layer  string `json:"layer"`
	RuleID string `json:"rule_id"`
	Kind   string `json:"kind"`
	// Pattern is the rule's text. It is left out of a match of the platform
	// policy that is shown to someone who may not read that policy, and
	// PatternHidden says so (see withoutPlatformPatterns).
	Pattern       string `json:"pattern"`
	PatternHidden bool   `json:"pattern_hidden,omitempty"`
	// Action is what the hit led to: block, replace or log.
	Action string `json:"action"`
	// Source is the class of the text that held it: primary, tool_output or
	// assistant_history.
	Source string `json:"source"`
	// Unit numbers the text in the request, in document order. Start and End
	// locate the hit in that text, in UTF-16 code units like a JavaScript
	// string index; NormalizedOnly says the text as written differs from what
	// matched (it was found only after case, width, zero-width characters and
	// whitespace were normalized, or, for a keyword that ignores separators,
	// with separators in it that the keyword lacks). They are filled in only
	// when asked for, as the panel's test dialog does.
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
	verdict := evaluateWordsUnits(extractWordUnits(format, payload), contexts, options)
	verdict.LatencyMS = time.Since(started).Milliseconds()
	return verdict
}

func evaluateWordsUnits(units []wordUnit, contexts []wordContext, options wordEvalOptions) WordVerdict {
	if options.budget == nil {
		options.budget = newRegexBudget()
	}
	verdict := scanWordUnits(units, contexts, options)
	verdict.RegexSkipped = options.budget.wasExceeded()
	return verdict
}

func scanWordUnits(units []wordUnit, contexts []wordContext, options wordEvalOptions) WordVerdict {
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
			if len(found) > 0 && context.action == WordActionReplace && !units[ui].signed {
				// A signed text is only recorded: see wordUnit.signed.
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
		rewritten, replaced, failure := rewriteWordUnits(units, contexts, replaceHits, normalizedText)
		if failure != nil {
			verdict.Action, verdict.Blocked, verdict.Reason, verdict.Field = WordVerdictBlock, true, failure.reason, failure.field
			verdict.BlockMessage = unrewritableMessage(firstReplaceContext(contexts).blockMessage, failure)
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

// withoutPlatformPatterns returns matches with the text of the platform policy's
// rules taken out, keeping the rule id. The platform's word list is the
// platform's to keep: a tenant may not read it (see the policy view), so it must
// not read it back out of a test result or of its own request log either. The
// argument is not changed.
func withoutPlatformPatterns(matches []WordMatch) []WordMatch {
	var out []WordMatch
	for i, match := range matches {
		if match.Layer == WordScopePlatform {
			if out == nil {
				out = append(make([]WordMatch, 0, len(matches)), matches[:i]...)
			}
			match.Pattern, match.PatternHidden = "", true
		}
		if out != nil {
			out = append(out, match)
		}
	}
	if out == nil {
		return matches
	}
	return out
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
		action := context.action
		if action == WordActionReplace && units[h.unit].signed {
			// Found, and recorded, but left as it is.
			action = WordActionLog
		}
		match := WordMatch{
			Layer:   rule.Layer,
			RuleID:  rule.ID,
			Kind:    rule.Kind,
			Pattern: rule.Pattern,
			Action:  action,
			Source:  units[h.unit].class.String(),
			Unit:    h.unit,
		}
		if options.positions {
			locateWordHit(&match, units[h.unit], context.fold, h.hit, normalizedText(h.unit, context.fold), rule)
		}
		matches = append(matches, match)
	}
	return matches
}

// locateWordHit fills in where a hit is in the text as written. rule is the one
// the hit is attributed to.
func locateWordHit(match *WordMatch, unit wordUnit, fold bool, hit wordHit, normalizedText string, rule wordRuleRef) {
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
	if rule.IgnoreSeparators && !match.NormalizedOnly {
		// The text is the same as the normalized text, but a keyword that ignores
		// separators matched it with something between its characters that the
		// keyword does not have: it is not the keyword as written either.
		match.NormalizedOnly = normalizedText[hit.start:hit.end] != normalizeForMatch(rule.Pattern, fold)
	}
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

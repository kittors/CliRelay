package contentmoderation

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// The word guard is a second, independent moderation layer next to the
// channel-bound Profile. A Profile hangs off a channel (an account or a
// provider key) and asks a moderation backend; a word policy hangs off the
// platform, a tenant or an API key and matches the operator's own words
// locally. Both can apply to one request.

// What a hit does.
const (
	// WordActionBlock rejects the request. It is the default: the safe choice
	// when an operator lists a word without saying what to do about it.
	WordActionBlock = "block"
	// WordActionReplace swaps each hit for the replacement text and lets the
	// request through.
	WordActionReplace = "replace"
	// WordActionLog lets the request through untouched and only records the hit
	// in the request log.
	WordActionLog = "log"
)

// Which text of a request is checked. Each level adds to the one before.
const (
	// WordScanLastUser checks only the newest user message.
	WordScanLastUser = "last_user"
	// WordScanAllUser checks every user message. It is the default: a client
	// resends the whole conversation and controls all of it, so a word put in an
	// earlier turn (followed by an innocent last message) would pass a check of
	// the last message only. The price is that a conversation that once held a
	// listed word is blocked again on every later request under the block action;
	// replace and log do not have that effect.
	WordScanAllUser = "all_user"
	// WordScanWithSystem adds system and developer prompts.
	WordScanWithSystem = "with_system"
	// WordScanWithTools adds everything else the client sends: tool results,
	// attached documents, tool-call arguments and replayed assistant turns.
	WordScanWithTools = "with_tools"
)

// The kinds of rule.
const (
	WordRuleKeyword = "keyword"
	WordRuleRegex   = "regex"
)

// Where a policy hangs.
const (
	// WordScopePlatform is the policy of the whole deployment. It is stored under
	// the system tenant and applies to every tenant's requests; no tenant can
	// switch it off or change it.
	WordScopePlatform = "platform"
	// WordScopeTenant is a tenant's default policy.
	WordScopeTenant = "tenant"
	// WordScopeKey adjusts the tenant policy for one API key.
	WordScopeKey = "key"
)

// Limits. They keep one policy from turning every request into a long scan and
// keep a saved policy within what a management request can carry.
const (
	// DefaultWordReplacement is what a replaced word becomes when the policy
	// names no text of its own.
	DefaultWordReplacement = "***"

	MaxWordRulesPerPolicy = 10000
	MaxWordRegexPerPolicy = 50
	// MaxWordRegexProgram bounds the compiled program of one regular expression,
	// in instructions, and MaxWordRegexProgramTotal the sum over a policy. The
	// source is short but its program is not: `(?:a{1000}){100}` is 16 bytes and
	// a hundred thousand instructions, and the engine pays the program's size for
	// every byte of text it reads. An ordinary expression (a phone number, an
	// address) is a few dozen.
	MaxWordRegexProgram      = 500
	MaxWordRegexProgramTotal = 2000
	// MaxWordKeywordRunes bounds a keyword; MaxWordRegexBytes a regular
	// expression's source.
	MaxWordKeywordRunes = 128
	MaxWordRegexBytes   = 512
	MaxWordNoteRunes    = 200
	// MaxWordRuleIDBytes bounds a rule id a client supplies. The server issues
	// uuids (36 bytes); the bound keeps a client from filling a policy with ids of
	// any length it likes.
	MaxWordRuleIDBytes = 64
	// MaxWordReplacementRunes bounds the replacement text.
	MaxWordReplacementRunes  = 64
	MaxWordBlockMessageRunes = 500
)

// ErrWordPolicyNotFound is returned for a policy that was never saved.
var ErrWordPolicyNotFound = errors.New("word policy not found")

// ErrWordPolicyVersionConflict is returned when a save names a version that is
// no longer the stored one.
var ErrWordPolicyVersionConflict = errors.New("word policy version conflict")

// WordRule is one keyword or regular expression.
type WordRule struct {
	// ID identifies the rule in logs and in the panel. The server assigns one to
	// a rule that has none.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Pattern is the keyword as the operator wrote it, or the regular
	// expression's source (Go RE2 syntax).
	Pattern string `json:"pattern"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note,omitempty"`
	// IgnoreSeparators makes a keyword match even with spaces, punctuation or
	// symbols between its characters (敏 感 词, 敏-感-词). Off unless asked for, and
	// for keywords only (see wordguard_separators.go).
	IgnoreSeparators bool `json:"ignore_separators,omitempty"`
}

// WordPolicy is the word guard configuration of one scope.
//
// Settings left empty (and the nil booleans) inherit. For a key they inherit
// from the tenant policy, and for the tenant and the platform from the
// defaults named on each constant.
type WordPolicy struct {
	TenantID  string `json:"-"`
	ScopeType string `json:"scope_type"`
	// ScopeID is the API key id for a key policy and empty otherwise.
	ScopeID string `json:"scope_id,omitempty"`
	// Enabled switches this policy's rules and settings off without deleting
	// them. For a key policy it only drops the key's own rules and settings;
	// the tenant and platform policies still apply.
	Enabled     bool   `json:"enabled"`
	Action      string `json:"action"`
	Replacement string `json:"replacement"`
	ScanScope   string `json:"scan_scope"`
	// ReplyFilter also filters what the model answers (see the reply filter).
	// Off by default.
	ReplyFilter *bool `json:"reply_filter"`
	// FoldTraditionalChinese folds Traditional characters onto Simplified before
	// matching, as the Profile keyword option does. Off by default.
	FoldTraditionalChinese *bool `json:"fold_traditional_chinese"`
	// InheritTenant says whether a key policy adds to the tenant policy's rules
	// (the default) or replaces them. It is ignored elsewhere. The platform
	// policy applies either way.
	InheritTenant bool `json:"inherit_tenant"`
	// BlockMessage is what a blocked client is told. Empty means the default.
	BlockMessage string     `json:"block_message"`
	Version      int64      `json:"version"`
	Rules        []WordRule `json:"rules"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// WordRuleProblem names one rule that cannot be saved and why.
type WordRuleProblem struct {
	// Index is the rule's position in the submitted list.
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	// Code is one of: empty, too_long, unknown_kind, invalid_regex,
	// matches_empty, regex_too_complex, too_many_rules, too_many_regex,
	// control_characters, invalid_text, only_separators, invalid_option.
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WordPolicyError reports why a policy was refused: a bad setting, or the
// rules that failed.
type WordPolicyError struct {
	Field   string            `json:"field,omitempty"`
	Message string            `json:"message"`
	Rules   []WordRuleProblem `json:"rules,omitempty"`
}

func (e *WordPolicyError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("invalid word policy: %s: %s", e.Field, e.Message)
	}
	if len(e.Rules) > 0 {
		first := e.Rules[0]
		return fmt.Sprintf("invalid word policy: rule %d (%q): %s", first.Index+1, first.Pattern, first.Message)
	}
	return "invalid word policy: " + e.Message
}

// Normalize checks the policy and puts it in the form that is stored: rules
// trimmed, given ids and deduplicated, defaults left empty. A policy with a
// bad setting or any bad rule is refused whole; the error names every rule that
// failed so an operator can fix a long import in one go.
func (p *WordPolicy) Normalize() error {
	p.ScopeType = strings.TrimSpace(p.ScopeType)
	p.ScopeID = strings.TrimSpace(p.ScopeID)
	switch p.ScopeType {
	case WordScopePlatform, WordScopeTenant:
		if p.ScopeID != "" {
			return &WordPolicyError{Field: "scope_id", Message: "only a key policy has a scope id"}
		}
	case WordScopeKey:
		if p.ScopeID == "" {
			return &WordPolicyError{Field: "scope_id", Message: "a key policy needs the API key id"}
		}
	default:
		return &WordPolicyError{Field: "scope_type", Message: "must be platform, tenant or key"}
	}
	p.Action = strings.TrimSpace(p.Action)
	switch p.Action {
	case "", WordActionBlock, WordActionReplace, WordActionLog:
	default:
		return &WordPolicyError{Field: "action", Message: "must be block, replace or log"}
	}
	p.ScanScope = strings.TrimSpace(p.ScanScope)
	switch p.ScanScope {
	case "", WordScanLastUser, WordScanAllUser, WordScanWithSystem, WordScanWithTools:
	default:
		return &WordPolicyError{Field: "scan_scope", Message: "must be last_user, all_user, with_system or with_tools"}
	}
	if err := validateFreeText("replacement", p.Replacement, MaxWordReplacementRunes, false); err != nil {
		return err
	}
	if err := validateFreeText("block_message", p.BlockMessage, MaxWordBlockMessageRunes, true); err != nil {
		return err
	}
	rules, problems := normalizeWordRules(p.Rules)
	if len(problems) > 0 {
		return &WordPolicyError{Message: "some rules are invalid", Rules: problems}
	}
	p.Rules = rules
	return nil
}

// validateFreeText checks a setting that is shown to people or inserted into
// their text. Replacement text must be a single printable run: a control
// character would be written into the middle of the client's own message.
func validateFreeText(field, value string, maxRunes int, allowNewline bool) error {
	if !utf8.ValidString(value) {
		return &WordPolicyError{Field: field, Message: "must be valid UTF-8"}
	}
	if utf8.RuneCountInString(value) > maxRunes {
		return &WordPolicyError{Field: field, Message: fmt.Sprintf("must be at most %d characters", maxRunes)}
	}
	for _, r := range value {
		if r < 0x20 && !(allowNewline && (r == '\n' || r == '\t')) || r == 0x7f {
			return &WordPolicyError{Field: field, Message: "must not contain control characters"}
		}
	}
	return nil
}

// normalizeWordRules validates a rule list and returns the list to store.
func normalizeWordRules(rules []WordRule) ([]WordRule, []WordRuleProblem) {
	var problems []WordRuleProblem
	if len(rules) > MaxWordRulesPerPolicy {
		problems = append(problems, WordRuleProblem{
			Index: MaxWordRulesPerPolicy, Code: "too_many_rules",
			Message: fmt.Sprintf("a policy holds at most %d rules", MaxWordRulesPerPolicy),
		})
		return nil, problems
	}
	out := make([]WordRule, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	usedIDs := make(map[string]struct{}, len(rules))
	regexCount, programTotal := 0, 0
	for index, rule := range rules {
		rule.Kind = strings.TrimSpace(rule.Kind)
		if rule.Kind == "" {
			rule.Kind = WordRuleKeyword
		}
		problem := func(code, message string) {
			problems = append(problems, WordRuleProblem{Index: index, ID: rule.ID, Kind: rule.Kind, Pattern: rule.Pattern, Code: code, Message: message})
		}
		if rule.Kind == WordRuleRegex {
			// Counted before the expression is compiled: a list of thousands of
			// expressions is refused without compiling any beyond the limit.
			if regexCount++; regexCount > MaxWordRegexPerPolicy {
				problem("too_many_regex", fmt.Sprintf("a policy holds at most %d regular expressions", MaxWordRegexPerPolicy))
				continue
			}
		}
		checked, code, message := checkWordRule(&rule)
		if code != "" {
			problem(code, message)
			continue
		}
		if _, duplicate := seen[checked.key]; duplicate {
			// The same rule twice means the same thing; keep the first.
			continue
		}
		if programTotal += checked.program; programTotal > MaxWordRegexProgramTotal {
			problem("regex_too_complex", fmt.Sprintf("the policy's regular expressions together compile to more than %d instructions; simplify or remove some (counted repetitions such as {100} are the usual cause)", MaxWordRegexProgramTotal))
			continue
		}
		seen[checked.key] = struct{}{}
		if _, taken := usedIDs[rule.ID]; rule.ID == "" || taken {
			rule.ID = uuid.NewString()
		}
		usedIDs[rule.ID] = struct{}{}
		out = append(out, rule)
	}
	return out, problems
}

// checkedWordRule is what checkWordRule learns about a rule that can be saved.
type checkedWordRule struct {
	// key is what two rules with the same effect share.
	key string
	// program is the size of a regular expression's compiled program, in
	// instructions; zero for a keyword.
	program int
}

// checkWordRule validates and trims one rule in place. It returns what the rule
// amounts to, or the code and message of the problem.
func checkWordRule(rule *WordRule) (checkedWordRule, string, string) {
	rule.ID = strings.TrimSpace(rule.ID)
	if len(rule.ID) > MaxWordRuleIDBytes {
		return checkedWordRule{}, "too_long", fmt.Sprintf("the rule id is longer than %d bytes", MaxWordRuleIDBytes)
	}
	rule.Note = strings.TrimSpace(rule.Note)
	if utf8.RuneCountInString(rule.Note) > MaxWordNoteRunes {
		return checkedWordRule{}, "too_long", fmt.Sprintf("the note is longer than %d characters", MaxWordNoteRunes)
	}
	if !utf8.ValidString(rule.ID) || !utf8.ValidString(rule.Pattern) || !utf8.ValidString(rule.Note) {
		return checkedWordRule{}, "invalid_text", "the rule must be valid UTF-8"
	}
	switch rule.Kind {
	case WordRuleKeyword:
		rule.Pattern = strings.TrimSpace(rule.Pattern)
		if rule.Pattern == "" {
			return checkedWordRule{}, "empty", "the keyword is empty"
		}
		if utf8.RuneCountInString(rule.Pattern) > MaxWordKeywordRunes {
			return checkedWordRule{}, "too_long", fmt.Sprintf("a keyword is at most %d characters", MaxWordKeywordRunes)
		}
		normalized := normalizeForMatch(rule.Pattern, false)
		if normalized == "" {
			// Only zero-width characters and spaces: it would match every request.
			return checkedWordRule{}, "empty", "the keyword has no visible characters"
		}
		if rule.IgnoreSeparators {
			stripped, _ := stripSeparators(normalized)
			if stripped == "" {
				return checkedWordRule{}, "only_separators", "the keyword is made of spaces and punctuation only, so with separators ignored there would be nothing to match"
			}
			// Written with or without its separators it is the same rule, but it is
			// not the rule of the same keyword that does not ignore them.
			return checkedWordRule{key: WordRuleKeyword + "\x00ignore-separators\x00" + stripped}, "", ""
		}
		return checkedWordRule{key: WordRuleKeyword + "\x00" + normalized}, "", ""
	case WordRuleRegex:
		if rule.IgnoreSeparators {
			return checkedWordRule{}, "invalid_option", "ignoring separators applies to keywords; a regular expression can match them itself"
		}
		// Only the line break a text file adds is stripped: a space can be part of
		// a pattern.
		rule.Pattern = strings.Trim(rule.Pattern, "\r\n")
		if rule.Pattern == "" {
			return checkedWordRule{}, "empty", "the regular expression is empty"
		}
		if strings.ContainsAny(rule.Pattern, "\r\n") {
			return checkedWordRule{}, "control_characters", "a regular expression must be on one line"
		}
		if len(rule.Pattern) > MaxWordRegexBytes {
			return checkedWordRule{}, "too_long", fmt.Sprintf("a regular expression is at most %d bytes", MaxWordRegexBytes)
		}
		compiled, code, message := compileWordRegex(rule.Pattern)
		if code != "" {
			return checkedWordRule{}, code, message
		}
		return checkedWordRule{key: WordRuleRegex + "\x00" + rule.Pattern, program: compiled.program}, "", ""
	default:
		return checkedWordRule{}, "unknown_kind", "the kind must be keyword or regex"
	}
}

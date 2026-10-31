package contentmoderation

import (
	"encoding/json"
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// PlatformWordTenantID is the tenant the platform word policy is stored under.
func PlatformWordTenantID() string { return platformWordTenantID() }

// WordDryRun is what the word guard would do with a text, for the panel's
// test dialog.
type WordDryRun struct {
	Action       string `json:"action"`
	Blocked      bool   `json:"blocked"`
	BlockMessage string `json:"block_message,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Output is the text as the upstream would receive it: the input with the
	// hits replaced under the replace action, the input itself otherwise.
	Output string `json:"output"`
	// Matches locate each hit in the input in UTF-16 code units, like a
	// JavaScript string index, and say whether it matched only after the text
	// was normalized (case, width, zero-width characters, white space).
	Matches    []WordMatch `json:"matches"`
	MatchCount int         `json:"match_count"`
	Replaced   int         `json:"replaced"`
	// Layers lists the policy layers whose rules were applied.
	Layers []string `json:"layers"`
	// ReplyFilter says whether any of those layers also filters the model's
	// reply, and ReplyOutput is what the same text would become in a reply.
	ReplyFilter bool   `json:"reply_filter"`
	ReplyOutput string `json:"reply_output,omitempty"`
	LatencyMS   int64  `json:"latency_ms"`
	// RegexSkipped says the text is larger than the work the regular expressions
	// are allowed, so some of them were not run on it (see regexBudget). A real
	// request would be checked the same way.
	RegexSkipped bool `json:"regex_skipped,omitempty"`
}

// DryRunWordPolicies runs text through the policies as a request from the user
// would be, without sending it anywhere.
func DryRunWordPolicies(policies WordPolicies, text string) WordDryRun {
	contexts := buildWordContexts(policies.Platform, policies.Tenant, policies.Key)
	result := WordDryRun{Action: WordVerdictAllow, Output: text, Matches: []WordMatch{}, Layers: []string{}}
	for _, context := range contexts {
		result.Layers = append(result.Layers, context.layer)
	}
	if len(contexts) == 0 || text == "" {
		return result
	}
	// A request body with the text as the one user message: the same path a real
	// request takes, so what the dialog shows is what a request would get.
	body, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
	verdict := evaluateWords(sdktranslator.FormatOpenAI, body, contexts, wordEvalOptions{positions: true})
	result.Action, result.Blocked, result.BlockMessage, result.Reason = verdict.Action, verdict.Blocked, verdict.BlockMessage, verdict.Reason
	result.Matches, result.MatchCount, result.Replaced, result.LatencyMS = verdict.Matches, verdict.MatchCount, verdict.Replaced, verdict.LatencyMS
	result.RegexSkipped = verdict.RegexSkipped
	if verdict.Payload != nil {
		result.Output = gjson.GetBytes(verdict.Payload, "messages.0.content").String()
	}
	if engine := newReplyEngine(contexts, nil); engine != nil {
		result.ReplyFilter = true
		result.ReplyOutput = engine.filterText(text, false).out
		result.RegexSkipped = result.RegexSkipped || engine.budget.wasExceeded()
	}
	return result
}

// Fates of a rule submitted for review.
const (
	// WordReviewNew: the rule is valid and not yet in the policy.
	WordReviewNew = "new"
	// WordReviewDuplicate: the same rule appears earlier in the submitted list.
	WordReviewDuplicate = "duplicate"
	// WordReviewExisting: the policy already has the rule.
	WordReviewExisting = "existing"
	// WordReviewInvalid: the rule cannot be saved.
	WordReviewInvalid = "invalid"
)

// WordRuleReview is the verdict on one submitted rule.
type WordRuleReview struct {
	Index  int    `json:"index"`
	Status string `json:"status"`
	// Rule is the rule as it would be stored (trimmed), for a new one. Program is
	// the size of its compiled regular expression (see MaxWordRegexProgramTotal).
	Rule    *WordRule `json:"rule,omitempty"`
	Program int       `json:"-"`
	// DuplicateOf is the index of the earlier submitted rule with the same effect.
	DuplicateOf *int `json:"duplicate_of,omitempty"`
	// ExistingID is the id of the rule the policy already has.
	ExistingID string           `json:"existing_id,omitempty"`
	Problem    *WordRuleProblem `json:"problem,omitempty"`
}

// WordRegexProgram is the size of a regular expression's compiled program, the
// unit MaxWordRegexProgramTotal is counted in, or zero when it cannot be
// compiled.
func WordRegexProgram(pattern string) int {
	compiled, code, _ := compileWordRegex(pattern)
	if code != "" {
		return 0
	}
	return compiled.program
}

// ReviewWordRules checks rules submitted for import the way a save would, and
// says what would become of each: stored, dropped as a repeat (of an earlier
// line or of a rule the policy already has) or refused, with the reason. It
// stores nothing.
func ReviewWordRules(incoming []WordRule, existing []WordRule) []WordRuleReview {
	have := make(map[string]string, len(existing))
	for _, rule := range existing {
		rule := rule
		if checked, code, _ := checkWordRule(&rule); code == "" {
			have[checked.key] = rule.ID
		}
	}
	seen := make(map[string]int, len(incoming))
	out := make([]WordRuleReview, 0, len(incoming))
	for index, rule := range incoming {
		if rule.Kind = strings.TrimSpace(rule.Kind); rule.Kind == "" {
			rule.Kind = WordRuleKeyword
		}
		review := WordRuleReview{Index: index}
		checked, code, message := checkWordRule(&rule)
		key := checked.key
		switch {
		case code != "":
			review.Status = WordReviewInvalid
			review.Problem = &WordRuleProblem{Index: index, ID: rule.ID, Kind: rule.Kind, Pattern: rule.Pattern, Code: code, Message: message}
		case seen[key] > 0:
			first := seen[key] - 1
			review.Status, review.DuplicateOf = WordReviewDuplicate, &first
		case have[key] != "":
			review.Status, review.ExistingID = WordReviewExisting, have[key]
			seen[key] = index + 1
		default:
			review.Status = WordReviewNew
			stored := rule
			review.Rule, review.Program = &stored, checked.program
			seen[key] = index + 1
		}
		out = append(out, review)
	}
	return out
}

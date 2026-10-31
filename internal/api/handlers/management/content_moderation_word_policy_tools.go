package management

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

// maxWordValidateRules bounds the rules one validation looks at: twice what a
// policy can hold, so that an import of a full list on top of a full policy is
// still judged (and turned down) rather than refused unread.
const maxWordValidateRules = 2 * contentmoderation.MaxWordRulesPerPolicy

// maxWordTestInputBytes bounds the text the test dialog checks.
const maxWordTestInputBytes = 1 << 20

// wordValidateRequest asks what saving the submitted rules into a policy would
// do with each of them.
type wordValidateRequest struct {
	// Scope is platform, tenant or key; the rules are compared with that
	// policy's stored rules. KeyID names the key for a key policy.
	Scope string          `json:"scope"`
	KeyID string          `json:"key_id"`
	Rules []wordRuleInput `json:"rules"`
}

// wordValidateSummary counts the verdicts and says whether the new rules fit.
type wordValidateSummary struct {
	Total     int `json:"total"`
	New       int `json:"new"`
	Duplicate int `json:"duplicate"`
	Existing  int `json:"existing"`
	Invalid   int `json:"invalid"`
	// ExistingRules and ExistingRegex count what the policy holds now; NewRegex
	// the regular expressions among the new rules.
	ExistingRules int `json:"existing_rules"`
	ExistingRegex int `json:"existing_regex"`
	NewRegex      int `json:"new_regex"`
	// Fits says whether adding the new rules to the stored ones stays within
	// the limits of a policy.
	Fits bool `json:"fits"`
}

// PostWordPolicyValidate checks rules for an import without storing anything:
// each rule is validated as a save would validate it (regular expressions
// compiled, empty and over-long rules named) and compared with the rules of the
// target policy and with the rules before it in the list. Rules that are fine
// and new are not listed; the panel has them in the file it parsed.
func (h *Handler) PostWordPolicyValidate(c *gin.Context) {
	var body wordValidateRequest
	if !readJSONBody(c, &body) {
		return
	}
	if len(body.Rules) > maxWordValidateRules {
		wordPolicyError(c, &contentmoderation.WordPolicyError{
			Message: "too many rules in one request",
			Rules: []contentmoderation.WordRuleProblem{{
				Index: maxWordValidateRules, Code: "too_many_rules",
				Message: "at most " + strconv.Itoa(maxWordValidateRules) + " rules can be checked at once; split the file",
			}},
		})
		return
	}
	scope := strings.TrimSpace(body.Scope)
	if scope == "" {
		scope = contentmoderation.WordScopeTenant
	}
	switch scope {
	case contentmoderation.WordScopePlatform, contentmoderation.WordScopeTenant, contentmoderation.WordScopeKey:
	default:
		contentModerationBadRequest(c, "scope must be platform, tenant or key")
		return
	}
	target, ok := h.wordPolicyTargetFor(c, scope, body.KeyID, true)
	if !ok {
		return
	}
	if !requireWordPolicyEditor(c, target) {
		return
	}
	var existing []contentmoderation.WordRule
	stored, err := h.contentModerationStore().GetWordPolicy(c.Request.Context(), target.tenantID, target.scopeType, target.scopeID)
	switch {
	case err == nil:
		existing = stored.Rules
	case !errors.Is(err, contentmoderation.ErrWordPolicyNotFound):
		wordPolicyError(c, err)
		return
	}

	reviews := contentmoderation.ReviewWordRules(wordRulesFromInput(body.Rules), existing)
	summary := wordValidateSummary{Total: len(reviews), ExistingRules: len(existing)}
	for _, rule := range existing {
		if rule.Kind == contentmoderation.WordRuleRegex {
			summary.ExistingRegex++
		}
	}
	listed := make([]contentmoderation.WordRuleReview, 0)
	for _, review := range reviews {
		switch review.Status {
		case contentmoderation.WordReviewNew:
			summary.New++
			if review.Rule != nil && review.Rule.Kind == contentmoderation.WordRuleRegex {
				summary.NewRegex++
			}
			continue
		case contentmoderation.WordReviewDuplicate:
			summary.Duplicate++
		case contentmoderation.WordReviewExisting:
			summary.Existing++
		case contentmoderation.WordReviewInvalid:
			summary.Invalid++
		}
		review.Rule = nil
		listed = append(listed, review)
	}
	summary.Fits = summary.ExistingRules+summary.New <= contentmoderation.MaxWordRulesPerPolicy &&
		summary.ExistingRegex+summary.NewRegex <= contentmoderation.MaxWordRegexPerPolicy
	c.JSON(http.StatusOK, gin.H{"summary": summary, "reviews": listed})
}

// wordTestRequest asks what the word guard would do with a text.
type wordTestRequest struct {
	Input string `json:"input"`
	// KeyID, when given, adds that API key's policy to the policies checked, as
	// a request made with the key would be.
	KeyID string `json:"key_id"`
	// Draft, when given, stands in for the stored policy of Scope (default
	// tenant), so unsaved edits can be tried before they are saved.
	Scope string           `json:"scope"`
	Draft *wordPolicyInput `json:"draft"`
}

// PostWordPolicyTest runs a text through the word policies that govern the
// effective tenant (platform, tenant and optionally one API key) the way a
// request would be, and reports the outcome without sending anything anywhere.
func (h *Handler) PostWordPolicyTest(c *gin.Context) {
	var body wordTestRequest
	if !readJSONBody(c, &body) {
		return
	}
	if strings.TrimSpace(body.Input) == "" {
		contentModerationBadRequest(c, "input is required")
		return
	}
	if len(body.Input) > maxWordTestInputBytes {
		contentModerationBadRequest(c, "input is too long to test")
		return
	}
	tenantID := effectiveTenantID(c)
	ctx, store := c.Request.Context(), h.contentModerationStore()
	load := func(tenant, scopeType, scopeID string) (*contentmoderation.WordPolicy, bool) {
		policy, err := store.GetWordPolicy(ctx, tenant, scopeType, scopeID)
		switch {
		case err == nil:
			return &policy, true
		case errors.Is(err, contentmoderation.ErrWordPolicyNotFound):
			return nil, true
		default:
			wordPolicyError(c, err)
			return nil, false
		}
	}

	var policies contentmoderation.WordPolicies
	var ok bool
	if policies.Platform, ok = load(contentmoderation.PlatformWordTenantID(), contentmoderation.WordScopePlatform, ""); !ok {
		return
	}
	if policies.Tenant, ok = load(tenantID, contentmoderation.WordScopeTenant, ""); !ok {
		return
	}
	keyID := strings.TrimSpace(body.KeyID)
	if keyID != "" {
		if h.apiKeySettings(c).GetRowByID(keyID) == nil {
			wordKeyNotFound(c)
			return
		}
		if policies.Key, ok = load(tenantID, contentmoderation.WordScopeKey, keyID); !ok {
			return
		}
	}

	if body.Draft != nil {
		scope := strings.TrimSpace(body.Scope)
		if scope == "" {
			scope = contentmoderation.WordScopeTenant
		}
		target := wordPolicyTarget{tenantID: tenantID, scopeType: scope}
		switch scope {
		case contentmoderation.WordScopePlatform:
			target.tenantID = contentmoderation.PlatformWordTenantID()
		case contentmoderation.WordScopeTenant:
		case contentmoderation.WordScopeKey:
			if keyID == "" {
				contentModerationBadRequest(c, "key_id is required to test a draft key policy")
				return
			}
			target.scopeID = keyID
		default:
			contentModerationBadRequest(c, "scope must be platform, tenant or key")
			return
		}
		// Not stored, but a tenant should not be able to see the platform layer
		// behave as something other than what the platform set.
		if !requireWordPolicyEditor(c, target) {
			return
		}
		draft := body.Draft.policy(target)
		if err := draft.Normalize(); err != nil {
			wordPolicyError(c, err)
			return
		}
		switch scope {
		case contentmoderation.WordScopePlatform:
			policies.Platform = &draft
		case contentmoderation.WordScopeTenant:
			policies.Tenant = &draft
		default:
			policies.Key = &draft
		}
	}
	c.JSON(http.StatusOK, contentmoderation.DryRunWordPolicies(policies, body.Input))
}

package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
)

// The word policies are the sensitive-word rules of the platform, of a tenant
// and of one of its API keys (see internal/contentmoderation). They are managed
// under /content-moderation/word-policies:
//
//	platform   the deployment-wide policy. Every tenant may read it; only a
//	           platform administrator working in the system tenant may change it.
//	tenant     the effective tenant's default policy.
//	keys/:id   the override of one API key of the effective tenant.
//
// A tenant can never switch the platform policy off or edit it: the platform
// layer is evaluated on its own, ahead of the tenant and key layers.

// The size of the largest request that can still be a legal one, which is what
// bounds a request body.
//
// A rule is at most an id, a keyword and a note (a regular expression has more
// to say but a policy holds few of them), and a client may write every
// character of them as a JSON escape: six bytes for most, twelve for a character
// outside the Basic Multilingual Plane, which takes a surrogate pair. Python's
// json.dumps does that to everything that is not ASCII by default.
const (
	jsonEscapeBytes     = 6
	jsonEscapePairBytes = 12
	// wordRuleJSONSlack covers the field names, quotes, commas and the white space
	// a pretty-printing client puts around one rule.
	wordRuleJSONSlack = 256

	maxKeywordRuleJSONBytes = jsonEscapeBytes*contentmoderation.MaxWordRuleIDBytes +
		jsonEscapePairBytes*(contentmoderation.MaxWordKeywordRunes+contentmoderation.MaxWordNoteRunes) + wordRuleJSONSlack
	maxRegexRuleJSONBytes = jsonEscapeBytes*(contentmoderation.MaxWordRuleIDBytes+contentmoderation.MaxWordRegexBytes) +
		jsonEscapePairBytes*contentmoderation.MaxWordNoteRunes + wordRuleJSONSlack
	// maxWordSettingsJSONBytes is the rest of a policy: the replacement and block
	// messages, and a few thousand bytes for its other fields.
	maxWordSettingsJSONBytes = jsonEscapePairBytes*(contentmoderation.MaxWordReplacementRunes+contentmoderation.MaxWordBlockMessageRunes) + 4096

	// maxWordPolicyBodyBytes bounds the body of a policy save, or of a validation
	// of the rules of one: the largest policy there can be, about 44 MiB with every
	// rule at its longest and every character escaped. A body larger than that
	// cannot hold a policy that would be stored, and one smaller is not refused
	// for its size.
	maxWordPolicyBodyBytes = (contentmoderation.MaxWordRulesPerPolicy-contentmoderation.MaxWordRegexPerPolicy)*maxKeywordRuleJSONBytes +
		contentmoderation.MaxWordRegexPerPolicy*maxRegexRuleJSONBytes + maxWordSettingsJSONBytes
)

// maxReportedRuleProblems bounds the rule problems listed in one error: an
// import of a wrong file would otherwise answer with a reply as large as the
// request.
const maxReportedRuleProblems = 200

// wordRuleInput is a rule as a client submits it. A rule that does not say
// otherwise is enabled and a keyword.
type wordRuleInput struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Pattern string `json:"pattern"`
	Enabled *bool  `json:"enabled"`
	Note    string `json:"note"`
	// IgnoreSeparators is for keywords only; a rule that does not say is strict.
	IgnoreSeparators bool `json:"ignore_separators"`
}

// wordRuleInputs is the rules of a request. It counts them as they are read and
// gives up at the first one past the limit, so that a body of millions of empty
// rules ({},{},{}...) is refused after ten thousand instead of being decoded
// whole: a decoded rule takes more than a hundred bytes of memory for the three
// it takes on the wire.
type wordRuleInputs []wordRuleInput

// tooManyWordRulesError is what decoding wordRuleInputs fails with when the list
// is longer than a request may carry.
type tooManyWordRulesError struct{ limit int }

func (e *tooManyWordRulesError) Error() string {
	return fmt.Sprintf("more than %d rules in one request", e.limit)
}

func (r *wordRuleInputs) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil { // JSON null
		*r = nil
		return nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return &json.UnmarshalTypeError{Value: "non-array", Type: reflect.TypeOf(*r)}
	}
	rules := wordRuleInputs{}
	for decoder.More() {
		if len(rules) == contentmoderation.MaxWordRulesPerPolicy {
			return &tooManyWordRulesError{limit: contentmoderation.MaxWordRulesPerPolicy}
		}
		var rule wordRuleInput
		if err := decoder.Decode(&rule); err != nil {
			return err
		}
		rules = append(rules, rule)
	}
	if _, err := decoder.Token(); err != nil { // the closing bracket
		return err
	}
	*r = rules
	return nil
}

func (r wordRuleInput) rule() contentmoderation.WordRule {
	return contentmoderation.WordRule{ID: r.ID, Kind: r.Kind, Pattern: r.Pattern, Enabled: r.Enabled == nil || *r.Enabled, Note: r.Note, IgnoreSeparators: r.IgnoreSeparators}
}

func wordRulesFromInput(in []wordRuleInput) []contentmoderation.WordRule {
	rules := make([]contentmoderation.WordRule, 0, len(in))
	for _, rule := range in {
		rules = append(rules, rule.rule())
	}
	return rules
}

// wordPolicyInput is the body of a policy save: the whole policy, which
// replaces the stored one. Version, when sent, is the version the client read;
// the save is refused if the policy has changed since.
type wordPolicyInput struct {
	Enabled                *bool          `json:"enabled"`
	Action                 string         `json:"action"`
	Replacement            string         `json:"replacement"`
	ScanScope              string         `json:"scan_scope"`
	ReplyFilter            *bool          `json:"reply_filter"`
	FoldTraditionalChinese *bool          `json:"fold_traditional_chinese"`
	InheritTenant          *bool          `json:"inherit_tenant"`
	BlockMessage           string         `json:"block_message"`
	Rules                  wordRuleInputs `json:"rules"`
	Version                *int64         `json:"version"`
}

// policy builds the policy a save would store, before it is checked.
func (in wordPolicyInput) policy(target wordPolicyTarget) contentmoderation.WordPolicy {
	return contentmoderation.WordPolicy{
		TenantID:               target.tenantID,
		ScopeType:              target.scopeType,
		ScopeID:                target.scopeID,
		Enabled:                in.Enabled == nil || *in.Enabled,
		Action:                 in.Action,
		Replacement:            in.Replacement,
		ScanScope:              in.ScanScope,
		ReplyFilter:            in.ReplyFilter,
		FoldTraditionalChinese: in.FoldTraditionalChinese,
		InheritTenant:          in.InheritTenant == nil || *in.InheritTenant,
		BlockMessage:           in.BlockMessage,
		Rules:                  wordRulesFromInput(in.Rules),
	}
}

// wordPolicyTarget is the policy a request addresses.
type wordPolicyTarget struct {
	// tenantID is the tenant the policy is stored under: the system tenant for
	// the platform policy, the effective tenant otherwise.
	tenantID  string
	scopeType string
	scopeID   string
	keyName   string
}

// wordPolicyResponse is a policy as the panel sees it.
type wordPolicyResponse struct {
	// Exists is false for a policy that was never saved; the rest then holds
	// what a first save would start from.
	Exists bool `json:"exists"`
	contentmoderation.WordPolicy
	// Effective is what the policy's empty settings resolve to.
	Effective contentmoderation.WordSettings `json:"effective"`
	// RuleCount is how many rules the policy has. RulesHidden says the rules are
	// not listed because the caller may not read them: the platform's word list is
	// kept from the tenants it is applied to, who see its settings and its size.
	RuleCount   int  `json:"rule_count"`
	RulesHidden bool `json:"rules_hidden"`
	// UnboundedRegexIDs lists the enabled regular expressions whose matches can
	// be of any length (\d+, \d{11,}). A reply that streams cannot have the start
	// of such a match held back, so a match split between two stream events can
	// get through; the panel says so next to them.
	UnboundedRegexIDs []string `json:"unbounded_regex_ids"`
	// CanEdit says whether the caller may change this policy. The panel offers
	// the platform policy read-only to everyone else.
	CanEdit bool   `json:"can_edit"`
	KeyName string `json:"key_name,omitempty"`
}

// canManageWordPlatform reports whether the caller may change the platform
// policy: a platform administrator acting in the system tenant, the same bar
// as for what the platform shares between tenants.
func canManageWordPlatform(c *gin.Context) bool {
	return effectiveTenantID(c) == identity.SystemTenantID && callerIsPlatformAdmin(c)
}

// wordPolicyTargetFor resolves the policy a request addresses and answers the
// request itself (returning false) when it cannot: a key that is not one of the
// effective tenant's. checkKey is false for a delete, which also clears the
// leftovers of a key that no longer exists.
func (h *Handler) wordPolicyTargetFor(c *gin.Context, scopeType, keyID string, checkKey bool) (wordPolicyTarget, bool) {
	switch scopeType {
	case contentmoderation.WordScopePlatform:
		return wordPolicyTarget{tenantID: contentmoderation.PlatformWordTenantID(), scopeType: scopeType}, true
	case contentmoderation.WordScopeKey:
		target := wordPolicyTarget{tenantID: effectiveTenantID(c), scopeType: scopeType, scopeID: strings.TrimSpace(keyID)}
		if target.scopeID == "" {
			contentModerationBadRequest(c, "the API key id is required")
			return wordPolicyTarget{}, false
		}
		if checkKey {
			row := h.apiKeySettings(c).GetRowByID(target.scopeID)
			if row == nil {
				wordKeyNotFound(c)
				return wordPolicyTarget{}, false
			}
			target.keyName = row.Name
		}
		return target, true
	default:
		return wordPolicyTarget{tenantID: effectiveTenantID(c), scopeType: contentmoderation.WordScopeTenant}, true
	}
}

// wordKeyNotFound answers a request that names an API key the effective tenant
// does not have, which is also what another tenant's key looks like.
func wordKeyNotFound(c *gin.Context) {
	c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "content_moderation_not_found", "message": "api key not found"}})
}

// requireWordPolicyEditor answers 403 (and returns false) when the caller may
// not change the policy. Tenant and key policies are open to anyone the route
// permission lets through; the platform policy needs more.
func requireWordPolicyEditor(c *gin.Context, target wordPolicyTarget) bool {
	if target.scopeType == contentmoderation.WordScopePlatform && !canManageWordPlatform(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
			"code":    "word_policy_platform_forbidden",
			"message": "only platform administrators working in the system tenant can change the platform word policy",
		}})
		return false
	}
	return true
}

// readJSONBody decodes a request body of at most limit bytes and answers the
// request itself (returning false) when it cannot.
func readJSONBody(c *gin.Context, v any, limit int64) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	err := json.NewDecoder(c.Request.Body).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	var tooMany *tooManyWordRulesError
	switch {
	case errors.As(err, &tooMany):
		// The same answer a save gives for a list that is too long once it has been
		// read, so the panel has one case to handle.
		wordPolicyError(c, &contentmoderation.WordPolicyError{
			Message: "too many rules in one request",
			Rules: []contentmoderation.WordRuleProblem{{
				Index: tooMany.limit, Code: "too_many_rules",
				Message: fmt.Sprintf("a request carries at most %d rules (a policy holds at most %d); split the list", tooMany.limit, contentmoderation.MaxWordRulesPerPolicy),
			}},
		})
	case errors.As(err, &tooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "word_policy_too_large", "message": "the request body is too large"}})
	case errors.Is(err, io.EOF):
		contentModerationBadRequest(c, "a JSON body is required")
	default:
		contentModerationBadRequest(c, "the body is not valid JSON: "+err.Error())
	}
	return false
}

// wordPolicyError answers a failed policy operation.
func wordPolicyError(c *gin.Context, err error) {
	var invalid *contentmoderation.WordPolicyError
	switch {
	case errors.As(err, &invalid):
		payload := gin.H{"code": "word_policy_invalid", "message": invalid.Error()}
		if invalid.Field != "" {
			payload["field"] = invalid.Field
		}
		if len(invalid.Rules) > 0 {
			payload["rules_total"] = len(invalid.Rules)
			payload["rules"] = invalid.Rules[:min(len(invalid.Rules), maxReportedRuleProblems)]
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": payload})
	case errors.Is(err, contentmoderation.ErrWordPolicyVersionConflict):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{
			"code":    "word_policy_version_conflict",
			"message": "the word policy was changed by someone else since it was loaded; reload it and apply the change again",
		}})
	case errors.Is(err, contentmoderation.ErrWordPolicyNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "word_policy_not_found", "message": "word policy not found"}})
	default:
		contentModerationError(c, err)
	}
}

// wordPolicyView builds the response for a policy. found says whether it is
// stored; if not, policy is replaced by the starting point of a first save.
func (h *Handler) wordPolicyView(ctx context.Context, c *gin.Context, store *contentmoderation.Store, target wordPolicyTarget, policy contentmoderation.WordPolicy, found bool) (wordPolicyResponse, error) {
	if !found {
		policy = contentmoderation.WordPolicy{
			TenantID: target.tenantID, ScopeType: target.scopeType, ScopeID: target.scopeID,
			Enabled: true, InheritTenant: true, Rules: []contentmoderation.WordRule{},
		}
	}
	if policy.Rules == nil {
		policy.Rules = []contentmoderation.WordRule{}
	}
	var below *contentmoderation.WordPolicy
	if target.scopeType == contentmoderation.WordScopeKey {
		tenant, err := store.GetWordPolicy(ctx, target.tenantID, contentmoderation.WordScopeTenant, "")
		switch {
		case err == nil:
			below = &tenant
		case !errors.Is(err, contentmoderation.ErrWordPolicyNotFound):
			return wordPolicyResponse{}, err
		}
	}
	ruleCount := len(policy.Rules)
	hidden := target.scopeType == contentmoderation.WordScopePlatform && !canManageWordPlatform(c)
	if hidden {
		policy.Rules = []contentmoderation.WordRule{}
	}
	unbounded := []string{}
	for _, rule := range policy.Rules {
		if rule.Enabled && rule.Kind == contentmoderation.WordRuleRegex && !contentmoderation.WordRegexIsBounded(rule.Pattern) {
			unbounded = append(unbounded, rule.ID)
		}
	}
	return wordPolicyResponse{
		Exists:            found,
		WordPolicy:        policy,
		Effective:         contentmoderation.ResolveWordSettings(below, &policy),
		RuleCount:         ruleCount,
		RulesHidden:       hidden,
		UnboundedRegexIDs: unbounded,
		CanEdit:           target.scopeType != contentmoderation.WordScopePlatform || canManageWordPlatform(c),
		KeyName:           target.keyName,
	}, nil
}

func (h *Handler) getWordPolicy(c *gin.Context, scopeType string) {
	target, ok := h.wordPolicyTargetFor(c, scopeType, c.Param("id"), true)
	if !ok {
		return
	}
	ctx, store := c.Request.Context(), h.contentModerationStore()
	policy, err := store.GetWordPolicy(ctx, target.tenantID, target.scopeType, target.scopeID)
	found := err == nil
	if err != nil && !errors.Is(err, contentmoderation.ErrWordPolicyNotFound) {
		wordPolicyError(c, err)
		return
	}
	view, err := h.wordPolicyView(ctx, c, store, target, policy, found)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func (h *Handler) putWordPolicy(c *gin.Context, scopeType string) {
	target, ok := h.wordPolicyTargetFor(c, scopeType, c.Param("id"), true)
	if !ok || !requireWordPolicyEditor(c, target) {
		return
	}
	var input wordPolicyInput
	if !readJSONBody(c, &input, maxWordPolicyBodyBytes) {
		return
	}
	policy := input.policy(target)
	if err := policy.Normalize(); err != nil {
		wordPolicyError(c, err)
		return
	}
	ctx, store := c.Request.Context(), h.contentModerationStore()
	saved, err := store.PutWordPolicy(ctx, policy, input.Version)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	view, err := h.wordPolicyView(ctx, c, store, target, saved, true)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

// deleteWordPolicy removes a policy. Removing one that is not there succeeds
// too: the caller wants it gone, and a leftover of a deleted key is cleared the
// same way.
func (h *Handler) deleteWordPolicy(c *gin.Context, scopeType string) {
	target, ok := h.wordPolicyTargetFor(c, scopeType, c.Param("id"), false)
	if !ok || !requireWordPolicyEditor(c, target) {
		return
	}
	err := h.contentModerationStore().DeleteWordPolicy(c.Request.Context(), target.tenantID, target.scopeType, target.scopeID)
	if err != nil && !errors.Is(err, contentmoderation.ErrWordPolicyNotFound) {
		wordPolicyError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetWordPolicyPlatform(c *gin.Context) {
	h.getWordPolicy(c, contentmoderation.WordScopePlatform)
}
func (h *Handler) PutWordPolicyPlatform(c *gin.Context) {
	h.putWordPolicy(c, contentmoderation.WordScopePlatform)
}
func (h *Handler) DeleteWordPolicyPlatform(c *gin.Context) {
	h.deleteWordPolicy(c, contentmoderation.WordScopePlatform)
}
func (h *Handler) GetWordPolicyTenant(c *gin.Context) {
	h.getWordPolicy(c, contentmoderation.WordScopeTenant)
}
func (h *Handler) PutWordPolicyTenant(c *gin.Context) {
	h.putWordPolicy(c, contentmoderation.WordScopeTenant)
}
func (h *Handler) DeleteWordPolicyTenant(c *gin.Context) {
	h.deleteWordPolicy(c, contentmoderation.WordScopeTenant)
}
func (h *Handler) GetWordPolicyKey(c *gin.Context) {
	h.getWordPolicy(c, contentmoderation.WordScopeKey)
}
func (h *Handler) PutWordPolicyKey(c *gin.Context) {
	h.putWordPolicy(c, contentmoderation.WordScopeKey)
}
func (h *Handler) DeleteWordPolicyKey(c *gin.Context) {
	h.deleteWordPolicy(c, contentmoderation.WordScopeKey)
}

// wordPolicyBrief is a policy without its rules. A policy that was never saved
// has Exists false and nothing else worth reading.
type wordPolicyBrief struct {
	Exists bool `json:"exists"`
	contentmoderation.WordPolicySummary
}

type wordKeyPolicyBrief struct {
	contentmoderation.WordPolicySummary
	// KeyID is the API key the policy belongs to; KeyName is its display name.
	KeyID   string `json:"key_id"`
	KeyName string `json:"key_name"`
}

// wordPolicyOverview is what the panel's word rules page loads first.
type wordPolicyOverview struct {
	// Defaults are what a setting left empty means; Limits bound a save.
	Defaults          contentmoderation.WordSettings     `json:"defaults"`
	Limits            contentmoderation.WordPolicyLimits `json:"limits"`
	CanManagePlatform bool                               `json:"can_manage_platform"`
	Platform          wordPolicyBrief                    `json:"platform"`
	Tenant            wordPolicyBrief                    `json:"tenant"`
	Keys              []wordKeyPolicyBrief               `json:"keys"`
}

// GetWordPolicies summarizes the policies that apply to the effective tenant:
// the platform's, the tenant's own and those of its API keys.
func (h *Handler) GetWordPolicies(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	summaries, err := h.contentModerationStore().ListWordPolicySummaries(c.Request.Context(), tenantID)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	names := map[string]string{}
	for _, row := range h.apiKeySettings(c).ListRows() {
		names[row.ID] = row.Name
	}
	overview := wordPolicyOverview{
		Defaults:          contentmoderation.WordDefaults(),
		Limits:            contentmoderation.WordLimits(),
		CanManagePlatform: canManageWordPlatform(c),
		Platform:          wordPolicyBrief{WordPolicySummary: contentmoderation.WordPolicySummary{ScopeType: contentmoderation.WordScopePlatform}},
		Tenant:            wordPolicyBrief{WordPolicySummary: contentmoderation.WordPolicySummary{ScopeType: contentmoderation.WordScopeTenant}},
		Keys:              []wordKeyPolicyBrief{},
	}
	for _, summary := range summaries {
		switch summary.ScopeType {
		case contentmoderation.WordScopePlatform:
			overview.Platform = wordPolicyBrief{Exists: true, WordPolicySummary: summary}
		case contentmoderation.WordScopeTenant:
			overview.Tenant = wordPolicyBrief{Exists: true, WordPolicySummary: summary}
		case contentmoderation.WordScopeKey:
			// The policy of a key that has since been deleted is not listed: nothing
			// can use it and the panel has no key to show it under.
			if name, exists := names[summary.ScopeID]; exists {
				overview.Keys = append(overview.Keys, wordKeyPolicyBrief{WordPolicySummary: summary, KeyID: summary.ScopeID, KeyName: name})
			}
		}
	}
	sort.SliceStable(overview.Keys, func(i, j int) bool {
		if overview.Keys[i].KeyName != overview.Keys[j].KeyName {
			return overview.Keys[i].KeyName < overview.Keys[j].KeyName
		}
		return overview.Keys[i].KeyID < overview.Keys[j].KeyID
	})
	c.JSON(http.StatusOK, overview)
}

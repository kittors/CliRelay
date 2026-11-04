package management

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	log "github.com/sirupsen/logrus"
)

// A portal user (an end user of a tenant, who signs in to the portal) may keep
// sensitive words of their own while the tenant allows it (see
// internal/contentmoderation/wordguard_portal.go). The tenant decides here:
//
//	GET  /content-moderation/word-policies/portal            the tenant's default and every portal user with their permission and policy
//	PUT  /content-moderation/word-policies/portal            the default
//	PUT  /content-moderation/word-policies/portal/users/:id  one user's override
//
// and may read and change a user's words under end-users/:id like any other
// policy. The user keeps them in the portal under /v0/portal/word-policy.

// endUserWordPolicyTarget resolves the policy of a portal user of the effective
// tenant. check is false for a delete, which also clears what a deleted user
// left behind.
func (h *Handler) endUserWordPolicyTarget(c *gin.Context, endUserID string, check bool) (wordPolicyTarget, bool) {
	target := wordPolicyTarget{tenantID: effectiveTenantID(c), scopeType: contentmoderation.WordScopeEndUser, scopeID: strings.TrimSpace(endUserID)}
	if target.scopeID == "" {
		contentModerationBadRequest(c, "the portal user id is required")
		return wordPolicyTarget{}, false
	}
	if !check {
		return target, true
	}
	user, ok := h.portalUserOfTenant(c, target.tenantID, target.scopeID)
	if !ok {
		return wordPolicyTarget{}, false
	}
	target.endUserName = endUserLabel(user.DisplayName, user.Username)
	return target, true
}

// portalUserOfTenant loads a portal user of the tenant, answering the request
// itself (returning false) when there is none: another tenant's user looks the
// same as one that does not exist.
func (h *Handler) portalUserOfTenant(c *gin.Context, tenantID, endUserID string) (enduser.User, bool) {
	svc := h.endUserService()
	if svc == nil {
		contentModerationError(c, contentmoderation.ErrUnavailable)
		return enduser.User{}, false
	}
	user, err := svc.GetUser(c.Request.Context(), tenantID, endUserID)
	if errors.Is(err, enduser.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "content_moderation_not_found", "message": "portal user not found"}})
		return enduser.User{}, false
	}
	if err != nil {
		contentModerationError(c, err)
		return enduser.User{}, false
	}
	return user, true
}

func endUserLabel(displayName, username string) string {
	if name := strings.TrimSpace(displayName); name != "" {
		return name
	}
	return username
}

func (h *Handler) GetWordPolicyEndUser(c *gin.Context) {
	h.getWordPolicy(c, contentmoderation.WordScopeEndUser)
}
func (h *Handler) PutWordPolicyEndUser(c *gin.Context) {
	h.putWordPolicy(c, contentmoderation.WordScopeEndUser)
}
func (h *Handler) DeleteWordPolicyEndUser(c *gin.Context) {
	h.deleteWordPolicy(c, contentmoderation.WordScopeEndUser)
}

// wordPortalUser is a portal user as the tenant's permission list shows it.
type wordPortalUser struct {
	enduser.UserBrief
	// Override is the user's own permission, nil when the tenant's default
	// applies; Allowed is what applies.
	Override *bool `json:"override"`
	Allowed  bool  `json:"allowed"`
	// Policy summarizes the user's words, nil when the user has none saved.
	Policy *contentmoderation.WordPolicySummary `json:"policy"`
}

// wordPortalView is the tenant's decision about its portal users' words.
type wordPortalView struct {
	DefaultAllowed bool                               `json:"default_allowed"`
	Limits         contentmoderation.WordPolicyLimits `json:"limits"`
	Users          []wordPortalUser                   `json:"users"`
}

// GetWordPortal lists the effective tenant's portal users with their
// permission and the summary of their words.
func (h *Handler) GetWordPortal(c *gin.Context) {
	ctx, store, tenantID := c.Request.Context(), h.contentModerationStore(), effectiveTenantID(c)
	access, err := store.GetWordPortalAccess(ctx, tenantID)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	summaries, err := store.ListWordPolicySummaries(ctx, tenantID)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	policies := map[string]contentmoderation.WordPolicySummary{}
	for _, summary := range summaries {
		if summary.ScopeType == contentmoderation.WordScopeEndUser {
			policies[summary.ScopeID] = summary
		}
	}
	svc := h.endUserService()
	if svc == nil {
		contentModerationError(c, contentmoderation.ErrUnavailable)
		return
	}
	briefs, err := svc.ListUserBriefs(ctx, tenantID)
	if err != nil && !errors.Is(err, enduser.ErrNotFound) {
		contentModerationError(c, err)
		return
	}
	view := wordPortalView{
		DefaultAllowed: access.DefaultAllowed,
		Limits:         contentmoderation.WordLimitsFor(contentmoderation.WordScopeEndUser),
		Users:          make([]wordPortalUser, 0, len(briefs)),
	}
	for _, brief := range briefs {
		user := wordPortalUser{UserBrief: brief, Allowed: access.Allowed(brief.ID)}
		if override, ok := access.Overrides[brief.ID]; ok {
			user.Override = &override
		}
		if summary, ok := policies[brief.ID]; ok {
			user.Policy = &summary
		}
		view.Users = append(view.Users, user)
	}
	// Users who keep words first: they are the ones a tenant looks for here.
	sort.SliceStable(view.Users, func(i, j int) bool {
		return (view.Users[i].Policy != nil) && (view.Users[j].Policy == nil)
	})
	c.JSON(http.StatusOK, view)
}

// PutWordPortal sets whether the effective tenant's portal users may keep
// words of their own, for every user without an override.
func (h *Handler) PutWordPortal(c *gin.Context) {
	var body struct {
		DefaultAllowed *bool `json:"default_allowed"`
	}
	if !readJSONBody(c, &body, 4096) {
		return
	}
	if body.DefaultAllowed == nil {
		contentModerationBadRequest(c, "default_allowed is required")
		return
	}
	if err := h.contentModerationStore().SetWordPortalDefault(c.Request.Context(), effectiveTenantID(c), *body.DefaultAllowed); err != nil {
		wordPolicyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"default_allowed": *body.DefaultAllowed})
}

// PutWordPortalUser sets one portal user's permission: allowed true or false
// whatever the tenant's default says, or null to follow the default again.
func (h *Handler) PutWordPortalUser(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	user, ok := h.portalUserOfTenant(c, tenantID, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Allowed *bool `json:"allowed"`
	}
	if !readJSONBody(c, &body, 4096) {
		return
	}
	ctx, store := c.Request.Context(), h.contentModerationStore()
	if err := store.SetWordPortalOverride(ctx, tenantID, user.ID, body.Allowed); err != nil {
		wordPolicyError(c, err)
		return
	}
	access, err := store.GetWordPortalAccess(ctx, tenantID)
	if err != nil {
		wordPolicyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": user.ID, "override": body.Allowed, "allowed": access.Allowed(user.ID)})
}

// forgetEndUserWords clears what the word guard keeps for a portal user who has
// just been deleted. The user is gone either way: a failure is logged, not
// answered.
func (h *Handler) forgetEndUserWords(c *gin.Context, tenantID, endUserID string) {
	if err := h.contentModerationStore().DeleteEndUserWordData(c.Request.Context(), tenantID, endUserID); err != nil && !errors.Is(err, contentmoderation.ErrUnavailable) {
		log.WithError(err).WithField("end_user_id", endUserID).Warn("could not remove the deleted portal user's sensitive words")
	}
}

// ---- The portal ----

// portalWordPolicyResponse is the signed-in portal user's own policy.
type portalWordPolicyResponse struct {
	wordPolicyResponse
	// Defaults are what a setting left empty means.
	Defaults contentmoderation.WordSettings `json:"defaults"`
	// GuardEnabled is false when the operator has switched the guard off: no
	// request is checked, whatever is saved.
	GuardEnabled bool `json:"guard_enabled"`
}

// portalWordPolicyTarget resolves the signed-in portal user's own policy, and
// answers the request itself (returning false) when the user may not keep one.
func (h *Handler) portalWordPolicyTarget(c *gin.Context) (wordPolicyTarget, bool) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return wordPolicyTarget{}, false
	}
	allowed, err := h.contentModerationStore().WordPortalAllowed(c.Request.Context(), user.TenantID, user.ID)
	if err != nil {
		wordPolicyError(c, err)
		return wordPolicyTarget{}, false
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{
			"code":    "word_policy_portal_forbidden",
			"message": "your administrator has not allowed you to set sensitive words",
		}})
		return wordPolicyTarget{}, false
	}
	return wordPolicyTarget{
		tenantID: user.TenantID, scopeType: contentmoderation.WordScopeEndUser, scopeID: user.ID,
		endUserName: endUserLabel(user.DisplayName, user.Username),
	}, true
}

// portalWordAllowed reports, for the portal's own account view, whether the
// signed-in user may keep sensitive words. It is false when it cannot be read:
// the portal then simply does not offer them.
func (h *Handler) portalWordAllowed(c *gin.Context, user enduser.User) bool {
	allowed, err := h.contentModerationStore().WordPortalAllowed(c.Request.Context(), user.TenantID, user.ID)
	return err == nil && allowed
}

func (h *Handler) GetPortalWordPolicy(c *gin.Context) {
	target, ok := h.portalWordPolicyTarget(c)
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
	c.JSON(http.StatusOK, portalWordPolicyResponse{
		wordPolicyResponse: view,
		Defaults:           contentmoderation.WordDefaults(),
		GuardEnabled:       contentmoderation.Runtime().WordGuardEnabled(),
	})
}

func (h *Handler) PutPortalWordPolicy(c *gin.Context) {
	if target, ok := h.portalWordPolicyTarget(c); ok {
		h.putWordPolicyOf(c, target)
	}
}

func (h *Handler) DeletePortalWordPolicy(c *gin.Context) {
	if target, ok := h.portalWordPolicyTarget(c); ok {
		h.deleteWordPolicyOf(c, target)
	}
}

// PostPortalWordPolicyValidate reviews rules for an import into the user's own
// policy, as the panel's import does for the other policies.
func (h *Handler) PostPortalWordPolicyValidate(c *gin.Context) {
	target, ok := h.portalWordPolicyTarget(c)
	if !ok {
		return
	}
	var body struct {
		Rules wordRuleInputs `json:"rules"`
	}
	if !readJSONBody(c, &body, maxWordPolicyBodyBytes) {
		return
	}
	h.answerWordRuleReview(c, target, body.Rules)
}

// PostPortalWordPolicyTest runs a text through the user's own policy, or the
// draft of it the user is editing. The tenant's and the platform's words are
// left out: the user may not read them, and a hit of theirs would give them
// away.
func (h *Handler) PostPortalWordPolicyTest(c *gin.Context) {
	target, ok := h.portalWordPolicyTarget(c)
	if !ok {
		return
	}
	var body struct {
		Input string           `json:"input"`
		Draft *wordPolicyInput `json:"draft"`
	}
	if !readJSONBody(c, &body, maxWordTestBodyBytes) {
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
	var policies contentmoderation.WordPolicies
	if body.Draft != nil {
		draft := body.Draft.policy(target)
		if err := draft.Normalize(); err != nil {
			wordPolicyError(c, err)
			return
		}
		policies.EndUser = &draft
	} else {
		stored, err := h.contentModerationStore().GetWordPolicy(c.Request.Context(), target.tenantID, target.scopeType, target.scopeID)
		switch {
		case err == nil:
			policies.EndUser = &stored
		case !errors.Is(err, contentmoderation.ErrWordPolicyNotFound):
			wordPolicyError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, contentmoderation.DryRunWordPolicies(policies, body.Input, false))
}

package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
)

// The portal's privacy masking: a portal user keeps a policy of their own for
// every request of their API keys, unless the tenant forbids it, and sees what
// was masked in their own requests whether or not they may.
//
//	GET/PUT/DELETE /v0/portal/privacy-masking/settings   the user's own policy; DELETE goes back to the tenant's
//	POST           /v0/portal/privacy-masking/preview    a text masked under the user's draft, nothing kept
//	GET            /v0/portal/privacy-masking/summary
//	GET            /v0/portal/privacy-masking/records
//	GET            /v0/portal/privacy-masking/records/:key
//	DELETE         /v0/portal/privacy-masking/records
//
// The tenant's words, rules and "never mask" list apply to the user's requests
// but are never shown to the user, and a preview leaves them out: a word the
// tenant keeps secret must not be found by trying texts out.

// portalMaskSettingsView is the signed-in user's settings.
type portalMaskSettingsView struct {
	// Policy is the user's own policy, or the tenant's default with its words,
	// rules and "never mask" list left out while the user has none.
	Policy contentmoderation.MaskPolicy `json:"policy"`
	// Own says the user keeps a policy of their own.
	Own bool `json:"own"`
	// Allowed says the user may keep one (change it, or remove it).
	Allowed bool `json:"allowed"`
	// Enforced are the categories the tenant keeps on for every portal user.
	Enforced []string `json:"enforced"`
	// PortalDefaultAllowed mirrors Allowed, for the panel's shared settings
	// form.
	PortalDefaultAllowed bool   `json:"portal_default_allowed"`
	Persistent           bool   `json:"persistent"`
	UpdatedAt            string `json:"updated_at,omitempty"`
}

// portalMaskUser is the signed-in user, their row, and the tenant's settings.
func (h *Handler) portalMaskUser(c *gin.Context) (enduser.User, contentmoderation.MaskUser, contentmoderation.SecretMaskingSettings, bool) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return enduser.User{}, contentmoderation.MaskUser{}, contentmoderation.SecretMaskingSettings{}, false
	}
	row, err := h.contentModerationStore().GetMaskUser(c.Request.Context(), user.TenantID, user.ID)
	if err != nil {
		privacyMaskError(c, err)
		return enduser.User{}, contentmoderation.MaskUser{}, contentmoderation.SecretMaskingSettings{}, false
	}
	return user, row, h.secretMaskingRegistry().For(user.TenantID), true
}

func (h *Handler) portalMaskSettingsView(row contentmoderation.MaskUser, settings contentmoderation.SecretMaskingSettings) portalMaskSettingsView {
	allowed := row.AllowedUnder(settings)
	view := portalMaskSettingsView{
		Own: row.Policy != nil && allowed, Allowed: allowed, Enforced: settings.Enforced,
		PortalDefaultAllowed: allowed, Persistent: h.secretMaskingRegistry().Persistent(), UpdatedAt: row.UpdatedAt,
	}
	if view.Own {
		view.Policy = *row.Policy
	} else {
		view.Policy = settings.PolicyView()
		view.Policy.Words, view.Policy.Rules, view.Policy.Allow = []string{}, []contentmoderation.MaskRule{}, []string{}
	}
	if err := view.Policy.Normalize(); err != nil {
		view.Policy = contentmoderation.MaskPolicy{}
		_ = view.Policy.Normalize()
	}
	if view.Enforced == nil {
		view.Enforced = []string{}
	}
	return view
}

// portalMaskingAllowed reports, for the portal's account view, whether the
// signed-in user may keep privacy masking settings of their own. False when
// it cannot be read: the portal then shows the records only.
func (h *Handler) portalMaskingAllowed(c *gin.Context, user enduser.User) bool {
	row, err := h.contentModerationStore().GetMaskUser(c.Request.Context(), user.TenantID, user.ID)
	return err == nil && row.AllowedUnder(h.secretMaskingRegistry().For(user.TenantID))
}

func (h *Handler) GetPortalPrivacyMaskingSettings(c *gin.Context) {
	_, row, settings, ok := h.portalMaskUser(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, h.portalMaskSettingsView(row, settings))
}

// portalMaskWrite answers a write the user may not make, returning false.
func portalMaskWrite(c *gin.Context, row contentmoderation.MaskUser, settings contentmoderation.SecretMaskingSettings) bool {
	if row.AllowedUnder(settings) {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "privacy_masking_portal_forbidden", "message": "your administrator has not allowed you to change your privacy masking settings"}})
	return false
}

func (h *Handler) PutPortalPrivacyMaskingSettings(c *gin.Context) {
	user, row, settings, ok := h.portalMaskUser(c)
	if !ok || !portalMaskWrite(c, row, settings) {
		return
	}
	var body struct {
		Policy *contentmoderation.MaskPolicy `json:"policy"`
	}
	if !readPrivacyMaskBody(c, &body, 1<<20) {
		return
	}
	if body.Policy == nil {
		privacyMaskBadRequest(c, "policy is required")
		return
	}
	policy := *body.Policy
	if err := policy.Normalize(); err != nil {
		privacyMaskError(c, err)
		return
	}
	ctx := c.Request.Context()
	// the user's requests are masked with the tenant's key, made now if the
	// tenant never set masking up
	if err := h.secretMaskingRegistry().EnsureKey(ctx, user.TenantID); err != nil {
		privacyMaskError(c, err)
		return
	}
	store := h.contentModerationStore()
	if err := store.PutMaskUserPolicy(ctx, user.TenantID, user.ID, &policy); err != nil {
		privacyMaskError(c, err)
		return
	}
	saved, err := store.GetMaskUser(ctx, user.TenantID, user.ID)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.portalMaskSettingsView(saved, h.secretMaskingRegistry().For(user.TenantID)))
}

func (h *Handler) DeletePortalPrivacyMaskingSettings(c *gin.Context) {
	user, row, settings, ok := h.portalMaskUser(c)
	if !ok || !portalMaskWrite(c, row, settings) {
		return
	}
	store := h.contentModerationStore()
	if err := store.PutMaskUserPolicy(c.Request.Context(), user.TenantID, user.ID, nil); err != nil {
		privacyMaskError(c, err)
		return
	}
	row.Policy = nil
	c.JSON(http.StatusOK, h.portalMaskSettingsView(row, settings))
}

// PostPortalPrivacyMaskingPreview masks a text under the user's draft (or
// saved) policy and what the tenant enforces, without the tenant's words,
// rules and "never mask" list.
func (h *Handler) PostPortalPrivacyMaskingPreview(c *gin.Context) {
	_, row, settings, ok := h.portalMaskUser(c)
	if !ok {
		return
	}
	var body struct {
		Text   string                        `json:"text"`
		Policy *contentmoderation.MaskPolicy `json:"policy"`
	}
	if !readPrivacyMaskBody(c, &body, 2*contentmoderation.MaxMaskPreviewBytes) {
		return
	}
	if len(body.Text) > contentmoderation.MaxMaskPreviewBytes {
		privacyMaskBadRequest(c, "the text is too long to try out")
		return
	}
	draft := h.portalMaskSettingsView(row, settings).Policy
	if body.Policy != nil {
		draft = *body.Policy
		if err := draft.Normalize(); err != nil {
			privacyMaskError(c, err)
			return
		}
	}
	tenant := contentmoderation.SecretMaskingSettings{Enforced: settings.Enforced}
	c.JSON(http.StatusOK, contentmoderation.PreviewMask(tenant, &draft, body.Text))
}

// portalMaskScope is the signed-in user's records.
func (h *Handler) portalMaskScope(c *gin.Context) (contentmoderation.MaskScope, bool) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return contentmoderation.MaskScope{}, false
	}
	return contentmoderation.MaskScope{TenantID: user.TenantID, EndUserID: user.ID}, true
}

func (h *Handler) GetPortalPrivacyMaskingSummary(c *gin.Context) {
	if scope, ok := h.portalMaskScope(c); ok {
		h.answerMaskSummary(c, scope)
	}
}

func (h *Handler) GetPortalPrivacyMaskingRecords(c *gin.Context) {
	if scope, ok := h.portalMaskScope(c); ok {
		h.answerMaskRecords(c, scope)
	}
}

func (h *Handler) GetPortalPrivacyMaskingRecord(c *gin.Context) {
	if scope, ok := h.portalMaskScope(c); ok {
		h.answerMaskRecord(c, scope)
	}
}

func (h *Handler) DeletePortalPrivacyMaskingRecords(c *gin.Context) {
	scope, ok := h.portalMaskScope(c)
	if !ok {
		return
	}
	deleted, err := h.contentModerationStore().DeleteMaskRecords(c.Request.Context(), scope)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

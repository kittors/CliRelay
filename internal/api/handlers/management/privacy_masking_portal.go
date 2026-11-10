package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
)

// The portal's privacy masking. What is masked is the tenant's call alone, so
// a portal user only sees it, and what was masked in their own requests:
//
//	GET  /v0/portal/privacy-masking/settings      the rules that apply, read only
//	POST /v0/portal/privacy-masking/preview       a text masked under them, nothing kept
//	GET  /v0/portal/privacy-masking/summary
//	GET  /v0/portal/privacy-masking/records
//	GET  /v0/portal/privacy-masking/records/:key
//
// From 2026-11-05 until the same day portal users also kept settings of their
// own and could delete their records; those endpoints are gone.
//
// The tenant's words, rules and "never mask" list apply to the user's requests
// but are never shown to the user, and a preview leaves them out: a word the
// tenant keeps secret must not be found by trying texts out.

// portalMaskSettingsView is what applies to the signed-in user's requests.
type portalMaskSettingsView struct {
	// Policy is the tenant's, without its words, rules and "never mask" list.
	Policy contentmoderation.MaskPolicy `json:"policy"`
	// ServerDisabled says the operator switched masking off for every tenant
	// (CLIRELAY_PRIVACY_MASK=off): nothing is masked whatever the policy says.
	ServerDisabled bool       `json:"server_disabled,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// portalMaskSettingsView is false while the tenant's settings are not known
// (see privacyMaskSettingsOf): the portal must not show "nothing is masked"
// while the user's requests are refused for not knowing.
func (h *Handler) portalMaskSettingsView(tenantID string) (portalMaskSettingsView, bool) {
	settings, known := h.secretMaskingRegistry().Lookup(tenantID)
	if !known {
		return portalMaskSettingsView{}, false
	}
	view := portalMaskSettingsView{Policy: settings.PolicyView(), ServerDisabled: contentmoderation.PrivacyMaskingDisabledByServer()}
	view.Policy.Words, view.Policy.Rules, view.Policy.Allow = []string{}, []contentmoderation.MaskRule{}, []string{}
	if err := view.Policy.Normalize(); err != nil {
		view.Policy = contentmoderation.MaskPolicy{}
		_ = view.Policy.Normalize()
	}
	if !settings.UpdatedAt.IsZero() {
		updated := settings.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view, true
}

func (h *Handler) GetPortalPrivacyMaskingSettings(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	view, known := h.portalMaskSettingsView(user.TenantID)
	if !known {
		privacyMaskError(c, contentmoderation.ErrUnavailable)
		return
	}
	c.JSON(http.StatusOK, view)
}

// PostPortalPrivacyMaskingPreview masks a text under the rules that apply to
// the user, without the tenant's words, rules and "never mask" list.
func (h *Handler) PostPortalPrivacyMaskingPreview(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !readPrivacyMaskBody(c, &body, 2*contentmoderation.MaxMaskPreviewBytes) {
		return
	}
	if len(body.Text) > contentmoderation.MaxMaskPreviewBytes {
		privacyMaskBadRequest(c, "the text is too long to try out")
		return
	}
	view, known := h.portalMaskSettingsView(user.TenantID)
	if !known {
		privacyMaskError(c, contentmoderation.ErrUnavailable)
		return
	}
	c.JSON(http.StatusOK, contentmoderation.PreviewMask(view.Policy, body.Text))
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

package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	log "github.com/sirupsen/logrus"
)

// Privacy masking (internal/privacymask, internal/contentmoderation
// secretmask*.go; ported from magpie, github.com/yetone/magpie): values found
// in a request are replaced with placeholders before it leaves and put back in
// the reply. A tenant sets it up here:
//
//	GET/PUT /privacy-masking/settings            the default policy, what is enforced for portal users, whether they may keep their own
//	POST    /privacy-masking/preview             a text masked under a policy, nothing kept
//	GET     /privacy-masking/portal              every portal user with their permission
//	PUT     /privacy-masking/portal/users/:id    one user's permission: allowed, forbidden, or null for the default
//	GET     /privacy-masking/summary             the records counted over the last days
//	GET     /privacy-masking/records             the records a page at a time
//	GET     /privacy-masking/records/:key        one record with its snippets
//	DELETE  /privacy-masking/records             every record (of one portal user or API key, when asked)
//
// Portal users keep their own under /v0/portal/privacy-masking
// (privacy_masking_portal.go). Nothing here ever returns a value masking
// found, or the tenant's placeholder key.

type privacyMaskSettingsView struct {
	Policy               contentmoderation.MaskPolicy `json:"policy"`
	Enforced             []string                     `json:"enforced"`
	PortalDefaultAllowed bool                         `json:"portal_default_allowed"`
	// Persistent is false when no settings store is attached.
	Persistent bool `json:"persistent"`
	// KeyProtected says the placeholder key is (or, once made, will be) stored
	// sealed under the server's data key.
	KeyProtected bool `json:"key_protected"`
	// ServerDisabled says the operator switched masking off for every tenant
	// (CLIRELAY_PRIVACY_MASK=off): nothing is masked whatever is set here.
	ServerDisabled bool       `json:"server_disabled,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

func (h *Handler) privacyMaskSettingsView(settings contentmoderation.SecretMaskingSettings) privacyMaskSettingsView {
	view := privacyMaskSettingsView{
		Policy:               settings.PolicyView(),
		Enforced:             settings.Enforced,
		PortalDefaultAllowed: settings.PortalAllowed(),
		Persistent:           h.secretMaskingRegistry().Persistent(),
		KeyProtected:         contentmoderation.DataKeyConfigured() && settings.Key == "",
		ServerDisabled:       contentmoderation.PrivacyMaskingDisabledByServer(),
	}
	if view.Enforced == nil {
		view.Enforced = []string{}
	}
	if !settings.UpdatedAt.IsZero() {
		updated := settings.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view
}

func (h *Handler) GetPrivacyMaskingSettings(c *gin.Context) {
	c.JSON(http.StatusOK, h.privacyMaskSettingsView(h.secretMaskingRegistry().For(effectiveTenantID(c))))
}

func (h *Handler) PutPrivacyMaskingSettings(c *gin.Context) {
	var body struct {
		Policy               *contentmoderation.MaskPolicy `json:"policy"`
		Enforced             []string                      `json:"enforced"`
		PortalDefaultAllowed *bool                         `json:"portal_default_allowed"`
	}
	if !readPrivacyMaskBody(c, &body, 1<<20) {
		return
	}
	if body.Policy == nil || body.PortalDefaultAllowed == nil {
		privacyMaskBadRequest(c, "policy and portal_default_allowed are required")
		return
	}
	policy := *body.Policy
	if err := policy.Normalize(); err != nil {
		privacyMaskError(c, err)
		return
	}
	enforced, err := contentmoderation.NormalizeEnforced(body.Enforced)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	saved, err := h.secretMaskingRegistry().SetPolicy(c.Request.Context(), effectiveTenantID(c), policy, enforced, *body.PortalDefaultAllowed)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.privacyMaskSettingsView(saved))
}

// PostPrivacyMaskingPreview masks a text under the policy sent (the panel's
// draft), or the tenant's saved one.
func (h *Handler) PostPrivacyMaskingPreview(c *gin.Context) {
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
	settings := h.secretMaskingRegistry().For(effectiveTenantID(c))
	if body.Policy != nil {
		draft := *body.Policy
		if err := draft.Normalize(); err != nil {
			privacyMaskError(c, err)
			return
		}
		settings = contentmoderation.SecretMaskingSettings{Policy: &draft}
	}
	c.JSON(http.StatusOK, contentmoderation.PreviewMask(settings, nil, body.Text))
}

// privacyMaskPortalUser is a portal user as the tenant's permission list shows
// them.
type privacyMaskPortalUser struct {
	enduser.UserBrief
	// Override is the user's own permission, nil when the default applies;
	// Allowed is what applies.
	Override  *bool `json:"override"`
	Allowed   bool  `json:"allowed"`
	HasPolicy bool  `json:"has_policy"`
}

// GetPrivacyMaskingPortal lists the tenant's portal users with whether each may
// keep a policy of their own, and has one.
func (h *Handler) GetPrivacyMaskingPortal(c *gin.Context) {
	ctx, tenantID := c.Request.Context(), effectiveTenantID(c)
	settings := h.secretMaskingRegistry().For(tenantID)
	stored, err := h.contentModerationStore().ListMaskUsers(ctx, tenantID)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	svc := h.endUserService()
	if svc == nil {
		privacyMaskError(c, contentmoderation.ErrUnavailable)
		return
	}
	briefs, err := svc.ListUserBriefs(ctx, tenantID)
	if err != nil && !errors.Is(err, enduser.ErrNotFound) {
		privacyMaskError(c, err)
		return
	}
	users := make([]privacyMaskPortalUser, 0, len(briefs))
	for _, brief := range briefs {
		row := stored[brief.ID]
		users = append(users, privacyMaskPortalUser{UserBrief: brief, Override: row.Allowed, Allowed: row.AllowedUnder(settings), HasPolicy: row.Policy != nil})
	}
	// users who keep a policy first: they are the ones a tenant looks for here
	sort.SliceStable(users, func(i, j int) bool { return users[i].HasPolicy && !users[j].HasPolicy })
	c.JSON(http.StatusOK, gin.H{"default_allowed": settings.PortalAllowed(), "users": users})
}

// PutPrivacyMaskingPortalUser sets one portal user's permission.
func (h *Handler) PutPrivacyMaskingPortalUser(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	user, ok := h.portalUserOfTenant(c, tenantID, c.Param("id"))
	if !ok {
		return
	}
	var body struct {
		Allowed *bool `json:"allowed"`
	}
	if !readPrivacyMaskBody(c, &body, 4096) {
		return
	}
	store := h.contentModerationStore()
	if err := store.SetMaskUserAllowed(c.Request.Context(), tenantID, user.ID, body.Allowed); err != nil {
		privacyMaskError(c, err)
		return
	}
	row, err := store.GetMaskUser(c.Request.Context(), tenantID, user.ID)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": user.ID, "override": body.Allowed, "allowed": row.AllowedUnder(h.secretMaskingRegistry().For(tenantID))})
}

// forgetEndUserPrivacyMasking clears what privacy masking keeps of a portal
// user who has just been deleted: their settings and their records.
func (h *Handler) forgetEndUserPrivacyMasking(c *gin.Context, tenantID, endUserID string) {
	if err := h.contentModerationStore().DeleteMaskUser(c.Request.Context(), tenantID, endUserID); err != nil && !errors.Is(err, contentmoderation.ErrUnavailable) {
		log.WithError(err).WithField("end_user_id", endUserID).Warn("could not remove the deleted portal user's privacy masking settings and records")
	}
}

// readPrivacyMaskBody decodes a body of at most limit bytes and answers the
// request itself (returning false) when it cannot.
func readPrivacyMaskBody(c *gin.Context, v any, limit int64) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	err := json.NewDecoder(c.Request.Body).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "privacy_masking_too_large", "message": "the request body is too large"}})
		return false
	}
	privacyMaskBadRequest(c, "the request body is not valid JSON for this endpoint")
	return false
}

func privacyMaskBadRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "privacy_masking_invalid_request", "message": message}})
}

// privacyMaskError answers a failure. Each of them leaves what is stored as
// it was, so the client may retry.
func privacyMaskError(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "privacy_masking_failed", "privacy masking could not complete the request"
	switch {
	case errors.Is(err, contentmoderation.ErrInvalidMaskPolicy):
		status, code, message = http.StatusBadRequest, "privacy_masking_invalid_settings", err.Error()
	case errors.Is(err, contentmoderation.ErrMaskRecordNotFound):
		status, code, message = http.StatusNotFound, "privacy_masking_not_found", "record not found"
	case errors.Is(err, contentmoderation.ErrSecretMaskingNotPersistent), errors.Is(err, contentmoderation.ErrUnavailable):
		status, code, message = http.StatusServiceUnavailable, "privacy_masking_unavailable", "privacy masking settings and records need the database, which is not available"
	case errors.Is(err, contentmoderation.ErrSecretMaskingConflict):
		status, code, message = http.StatusConflict, "privacy_masking_conflict", "the settings were changed by someone else while saving; reload and try again"
	default:
		log.WithError(err).Warn("privacy masking: a management request failed")
	}
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

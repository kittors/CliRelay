package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	log "github.com/sirupsen/logrus"
)

// Privacy masking (internal/privacymask, internal/contentmoderation
// secretmask*.go; ported from magpie, github.com/yetone/magpie): values found
// in a request are replaced with placeholders before it leaves and put back in
// the reply. What is masked is the tenant's call, set up here:
//
//	GET/PUT /privacy-masking/settings            the tenant's policy
//	POST    /privacy-masking/preview             a text masked under a policy, nothing kept
//	GET     /privacy-masking/summary             the records counted over the last days
//	GET     /privacy-masking/records             the records a page at a time
//	GET     /privacy-masking/records/:key        one record with its snippets
//	DELETE  /privacy-masking/records             every record (of one portal user or API key, when asked)
//
// Portal users only see what applies to them and their own records, under
// /v0/portal/privacy-masking (privacy_masking_portal.go); they set nothing.
// Nothing here ever returns a value masking found, or the tenant's
// placeholder key.

type privacyMaskSettingsView struct {
	Policy contentmoderation.MaskPolicy `json:"policy"`
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
		Policy:         settings.PolicyView(),
		Persistent:     h.secretMaskingRegistry().Persistent(),
		KeyProtected:   contentmoderation.DataKeyConfigured() && settings.Key == "",
		ServerDisabled: contentmoderation.PrivacyMaskingDisabledByServer(),
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
	saved, err := h.secretMaskingRegistry().SetPolicy(c.Request.Context(), effectiveTenantID(c), policy)
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
	policy := h.secretMaskingRegistry().For(effectiveTenantID(c)).PolicyView()
	if body.Policy != nil {
		policy = *body.Policy
		if err := policy.Normalize(); err != nil {
			privacyMaskError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, contentmoderation.PreviewMask(policy, body.Text))
}

// forgetEndUserPrivacyMasking removes the records of a portal user who has
// just been deleted.
func (h *Handler) forgetEndUserPrivacyMasking(c *gin.Context, tenantID, endUserID string) {
	if err := h.contentModerationStore().DeleteMaskUserRecords(c.Request.Context(), tenantID, endUserID); err != nil && !errors.Is(err, contentmoderation.ErrUnavailable) {
		log.WithError(err).WithField("end_user_id", endUserID).Warn("could not remove the deleted portal user's privacy masking records")
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

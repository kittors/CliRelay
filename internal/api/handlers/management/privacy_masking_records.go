package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/privacymask"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// The records of privacy masking, for the tenant (here) and for a portal user
// (privacy_masking_portal.go): the same queries, scoped to the user there.

// maskRecordFacets are the facets a list can be narrowed to.
var maskRecordFacets = map[string]bool{"secret": true, "personal": true, "word": true, "rule": true, "warn": true, "block": true}

// privacyMaskDays reads ?days=, 7 or 30.
func privacyMaskDays(c *gin.Context) int {
	if strings.TrimSpace(c.Query("days")) == "30" {
		return 30
	}
	return 7
}

// privacyMaskQuery reads the list's filters, answering the request itself
// (returning false) when one is not valid.
func privacyMaskQuery(c *gin.Context) (contentmoderation.MaskRecordQuery, bool) {
	q := contentmoderation.MaskRecordQuery{
		Days:    privacyMaskDays(c),
		Kind:    strings.TrimSpace(c.Query("kind")),
		Facet:   strings.TrimSpace(c.Query("facet")),
		Client:  strings.TrimSpace(c.Query("client")),
		Channel: strings.TrimSpace(c.Query("channel")),
		Model:   strings.TrimSpace(c.Query("model")),
		Before:  strings.TrimSpace(c.Query("before")),
		Limit:   queryPositiveInt(c, "limit", 50),
	}
	switch {
	case q.Kind != "" && !privacymask.KindName(q.Kind):
		privacyMaskBadRequest(c, "kind is not a kind a value can have")
	case q.Facet != "" && !maskRecordFacets[q.Facet]:
		privacyMaskBadRequest(c, "facet is one of secret, personal, word, rule, warn and block")
	case q.Before != "" && !maskRecordKey(q.Before):
		privacyMaskBadRequest(c, "before is the key of a record")
	default:
		return q, true
	}
	return q, false
}

// maskRecordKey says key is written as a record's key is: 24 hex digits.
func maskRecordKey(key string) bool {
	if len(key) != 24 {
		return false
	}
	for _, r := range key {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// adminMaskScope is the tenant's records, or one portal user's or API key's
// when the query asks.
func adminMaskScope(c *gin.Context) contentmoderation.MaskScope {
	return contentmoderation.MaskScope{
		TenantID:  effectiveTenantID(c),
		EndUserID: strings.TrimSpace(c.Query("end_user_id")),
		APIKeyID:  strings.TrimSpace(c.Query("api_key_id")),
	}
}

func (h *Handler) GetPrivacyMaskingSummary(c *gin.Context) {
	h.answerMaskSummary(c, adminMaskScope(c))
}

func (h *Handler) GetPrivacyMaskingRecords(c *gin.Context) {
	h.answerMaskRecords(c, adminMaskScope(c))
}

func (h *Handler) GetPrivacyMaskingRecord(c *gin.Context) {
	h.answerMaskRecord(c, contentmoderation.MaskScope{TenantID: effectiveTenantID(c)})
}

// DeletePrivacyMaskingRecords removes the tenant's records, or one portal
// user's or API key's, and notes it in the audit log.
func (h *Handler) DeletePrivacyMaskingRecords(c *gin.Context) {
	scope := adminMaskScope(c)
	deleted, err := h.contentModerationStore().DeleteMaskRecords(c.Request.Context(), scope)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	if principal, ok := principalFromContext(c); ok && h.identity() != nil {
		h.identity().RecordAudit(c.Request.Context(), identity.AuditEvent{
			TenantID: principal.EffectiveTenant.ID, ActorKind: principal.Kind, ActorUserID: principal.User.ID, ActorSessionID: principal.SessionID,
			Action: "privacy_masking.records.delete", ResourceType: "privacy_mask_records", ResourceID: scope.TenantID, Result: "success",
			Changes: map[string]any{"deleted": deleted, "end_user_id": scope.EndUserID, "api_key_id": scope.APIKeyID},
		})
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) answerMaskSummary(c *gin.Context, scope contentmoderation.MaskScope) {
	summary, err := h.contentModerationStore().MaskSummary(c.Request.Context(), scope, privacyMaskDays(c), time.Now())
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, summary)
}

func (h *Handler) answerMaskRecords(c *gin.Context, scope contentmoderation.MaskScope) {
	q, ok := privacyMaskQuery(c)
	if !ok {
		return
	}
	page, err := h.contentModerationStore().MaskRecords(c.Request.Context(), scope, q, time.Now())
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	h.nameMaskRecords(c, scope.TenantID, page.Items)
	c.JSON(http.StatusOK, page)
}

func (h *Handler) answerMaskRecord(c *gin.Context, scope contentmoderation.MaskScope) {
	key := strings.TrimSpace(c.Param("key"))
	if !maskRecordKey(key) {
		privacyMaskError(c, contentmoderation.ErrMaskRecordNotFound)
		return
	}
	record, err := h.contentModerationStore().MaskRecord(c.Request.Context(), scope, key)
	if err != nil {
		privacyMaskError(c, err)
		return
	}
	records := []contentmoderation.MaskRecord{record}
	h.nameMaskRecords(c, scope.TenantID, records)
	c.JSON(http.StatusOK, records[0])
}

// nameMaskRecords fills in the names of the API keys and the portal users of
// records: the records keep ids, and show the names as they are now.
func (h *Handler) nameMaskRecords(c *gin.Context, tenantID string, records []contentmoderation.MaskRecord) {
	if len(records) == 0 {
		return
	}
	keys := map[string]string{}
	for _, row := range usage.ListAPIKeysForTenant(tenantID) {
		keys[strings.TrimSpace(row.ID)] = strings.TrimSpace(row.Name)
	}
	users := map[string]string{}
	needUsers := false
	for _, r := range records {
		needUsers = needUsers || r.EndUserID != ""
	}
	if svc := h.endUserService(); needUsers && svc != nil {
		if briefs, err := svc.ListUserBriefs(c.Request.Context(), tenantID); err == nil {
			for _, brief := range briefs {
				users[brief.ID] = endUserLabel(brief.DisplayName, brief.Username)
			}
		}
	}
	for i := range records {
		records[i].APIKeyName = keys[records[i].APIKeyID]
		records[i].EndUser = users[records[i].EndUserID]
	}
}

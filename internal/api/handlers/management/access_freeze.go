package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accessfreeze"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// Per-provider access freezes (package accessfreeze): when an upstream refuses
// a requester's request for its content, the gateway freezes that requester's
// access to the provider. The routes live under /end-users so they share the
// portal accounts' permissions (end_users.read / end_users.write) and tenant
// scope; a freeze of an API key no portal account owns is listed here too.

// maxReleaseNote bounds the note an administrator leaves on a release.
const maxReleaseNote = 500

// accessFreezeView is a freeze as the panel reads it: the stored fields, the
// requester's name, the vendor name of the provider ("" for an operator-named
// upstream), and the notice the requester is answered with.
type accessFreezeView struct {
	accessfreeze.Freeze
	SubjectName  string `json:"subject_name"`
	ProviderName string `json:"provider_name"`
	Notice       string `json:"notice"`
}

func newAccessFreezeView(f accessfreeze.Freeze, subjectName string) accessFreezeView {
	name, _ := accessfreeze.ProviderDisplayName(f.Provider)
	return accessFreezeView{Freeze: f, SubjectName: subjectName, ProviderName: name, Notice: accessfreeze.NoticeFor(f)}
}

// accessFreezeSubjectNames names each requester of a listing once: a portal
// account by its display name (or username), a key by its name, "" when it
// is gone. Without it the panel had to load every API key, secrets included,
// to name the one frozen key.
func (h *Handler) accessFreezeSubjectNames(ctx context.Context, tenantID string, subjects []accessfreeze.Subject) map[accessfreeze.Subject]string {
	names := make(map[accessfreeze.Subject]string, len(subjects))
	users := h.endUserService()
	for _, subject := range subjects {
		if _, seen := names[subject]; seen {
			continue
		}
		name := ""
		switch subject.Kind {
		case accessfreeze.SubjectEndUser:
			if users != nil {
				if user, err := users.GetUser(ctx, tenantID, subject.ID); err == nil {
					name = strings.TrimSpace(user.DisplayName)
					if name == "" {
						name = strings.TrimSpace(user.Username)
					}
				}
			}
		case accessfreeze.SubjectAPIKey:
			if usage.RuntimeDB() != nil {
				if row := usage.GetAPIKeyByIDForTenant(tenantID, subject.ID); row != nil {
					name = strings.TrimSpace(row.Name)
				}
			}
		}
		names[subject] = name
	}
	return names
}

func accessFreezeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, accessfreeze.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "access_freeze_not_found", "message": "access freeze not found"}})
	case errors.Is(err, accessfreeze.ErrAlreadyReleased):
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "access_freeze_already_released", "message": "access freeze already released"}})
	case errors.Is(err, accessfreeze.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "access_freezes_unavailable", "message": "access freezes are unavailable without the runtime database"}})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "access_freeze_failed", "message": err.Error()}})
	}
}

func accessFreezeBadRequest(c *gin.Context, code, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": code, "message": message}})
}

// accessFreezeStore reads and writes the freezes in the runtime database. The
// installed service's store is the same database; a process that did not
// start through cmd (the route smoke tests) has no service, and the
// management API still answers from the database, as contentModerationStore
// does. Without a database the store answers ErrUnavailable (503).
func accessFreezeStore() *accessfreeze.Store {
	if service := accessfreeze.Runtime(); service != nil {
		return service.Store()
	}
	return accessfreeze.NewStore(usage.RuntimeDB())
}

func requireEndUserWrite(c *gin.Context) bool {
	principal, _ := principalFromContext(c)
	if !principal.Has("end_users.write") && !principal.PlatformAdmin {
		identityError(c, identity.ErrPermissionDenied)
		return false
	}
	return true
}

func parseSubjectKind(raw string) (accessfreeze.SubjectKind, bool) {
	kind := accessfreeze.SubjectKind(strings.TrimSpace(raw))
	return kind, kind == "" || kind.Valid()
}

// GetAccessFreezes lists the tenant's freezes, newest first.
// Query: status (active | released | all, default all), subject_kind,
// subject_id, provider, limit.
func (h *Handler) GetAccessFreezes(c *gin.Context) {
	status := strings.TrimSpace(c.Query("status"))
	switch status {
	case "all":
		status = ""
	case "", "active", "released":
	default:
		accessFreezeBadRequest(c, "invalid_status", "status must be active, released or all")
		return
	}
	kind, ok := parseSubjectKind(c.Query("subject_kind"))
	if !ok {
		accessFreezeBadRequest(c, "invalid_subject_kind", "subject_kind must be end_user or api_key")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	tenantID := effectiveTenantID(c)
	freezes, err := accessFreezeStore().List(c.Request.Context(), tenantID, accessfreeze.Filter{
		Status: status, SubjectKind: kind, SubjectID: c.Query("subject_id"), Provider: c.Query("provider"), Limit: limit,
	})
	if err != nil {
		accessFreezeError(c, err)
		return
	}
	subjects := make([]accessfreeze.Subject, 0, len(freezes))
	for _, f := range freezes {
		subjects = append(subjects, f.Subject)
	}
	names := h.accessFreezeSubjectNames(c.Request.Context(), tenantID, subjects)
	items := make([]accessFreezeView, 0, len(freezes))
	for _, f := range freezes {
		items = append(items, newAccessFreezeView(f, names[f.Subject]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// GetAccessFreezeStrikes lists one requester's refusals, newest first: the
// evidence its freezes rest on. Query: subject_kind and subject_id (required),
// provider, limit.
func (h *Handler) GetAccessFreezeStrikes(c *gin.Context) {
	kind, ok := parseSubjectKind(c.Query("subject_kind"))
	subjectID := strings.TrimSpace(c.Query("subject_id"))
	if !ok || kind == "" || subjectID == "" {
		accessFreezeBadRequest(c, "invalid_subject", "subject_kind (end_user or api_key) and subject_id are required")
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	subject := accessfreeze.Subject{TenantID: effectiveTenantID(c), Kind: kind, ID: subjectID}
	strikes, err := accessFreezeStore().ListStrikes(c.Request.Context(), subject, c.Query("provider"), limit)
	if err != nil {
		accessFreezeError(c, err)
		return
	}
	items := make([]gin.H, 0, len(strikes))
	for _, st := range strikes {
		name, _ := accessfreeze.ProviderDisplayName(st.Provider)
		items = append(items, gin.H{
			"id": st.ID, "subject": st.Subject, "provider": st.Provider, "provider_name": name,
			"reason_code": st.ReasonCode, "upstream_message": st.UpstreamMessage, "model": st.Model,
			"auth_label": st.AuthLabel, "request_id": st.RequestID, "created_at": st.CreatedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// PostAccessFreezeRelease lifts a freeze. Body: {"note": "..."} (optional).
// The requester can use the provider again from its next request.
func (h *Handler) PostAccessFreezeRelease(c *gin.Context) {
	if !requireEndUserWrite(c) {
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("freeze_id")), 10, 64)
	if err != nil || id <= 0 {
		accessFreezeError(c, accessfreeze.ErrNotFound)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&body); errBind != nil {
			accessFreezeBadRequest(c, "invalid_body", "body must be a JSON object with an optional note")
			return
		}
	}
	note := strings.TrimSpace(body.Note)
	if len([]rune(note)) > maxReleaseNote {
		accessFreezeBadRequest(c, "note_too_long", "note must be at most 500 characters")
		return
	}
	actor := periodResetActorFromContext(c)
	by := actor.Username
	if by == "" {
		by = actor.UserID
	}
	if by == "" {
		by = actor.Kind
	}
	tenantID := effectiveTenantID(c)
	freeze, err := accessfreeze.Release(c.Request.Context(), accessFreezeStore(), tenantID, id, by, note)
	if err != nil {
		accessFreezeError(c, err)
		return
	}
	names := h.accessFreezeSubjectNames(c.Request.Context(), tenantID, []accessfreeze.Subject{freeze.Subject})
	c.JSON(http.StatusOK, gin.H{"freeze": newAccessFreezeView(freeze, names[freeze.Subject])})
}

func accessFreezeSettingsBody(threshold int) gin.H {
	return gin.H{
		"threshold":         threshold,
		"default_threshold": accessfreeze.DefaultThreshold,
		"max_threshold":     accessfreeze.MaxThreshold,
	}
}

// GetAccessFreezeSettings returns the tenant's threshold: how many content
// refusals since the last release freeze a requester's access to a provider.
// 0 records the refusals without freezing.
func (h *Handler) GetAccessFreezeSettings(c *gin.Context) {
	threshold, err := accessFreezeStore().Threshold(c.Request.Context(), effectiveTenantID(c))
	if err != nil {
		accessFreezeError(c, err)
		return
	}
	c.JSON(http.StatusOK, accessFreezeSettingsBody(threshold))
}

// PutAccessFreezeSettings sets the tenant's threshold. Body: {"threshold": n}.
func (h *Handler) PutAccessFreezeSettings(c *gin.Context) {
	if !requireEndUserWrite(c) {
		return
	}
	var body struct {
		Threshold *int `json:"threshold"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Threshold == nil {
		accessFreezeBadRequest(c, "invalid_threshold", "body must be {\"threshold\": n}")
		return
	}
	if *body.Threshold < 0 || *body.Threshold > accessfreeze.MaxThreshold {
		accessFreezeBadRequest(c, "invalid_threshold", "threshold must be between 0 and 100")
		return
	}
	actor := periodResetActorFromContext(c)
	by := actor.Username
	if by == "" {
		by = actor.UserID
	}
	if err := accessFreezeStore().SetThreshold(c.Request.Context(), effectiveTenantID(c), *body.Threshold, by, time.Now()); err != nil {
		accessFreezeError(c, err)
		return
	}
	c.JSON(http.StatusOK, accessFreezeSettingsBody(*body.Threshold))
}

// portalAccessFreezeView is a standing freeze as its portal user reads it. It
// leaves out the upstream account and the request id: the portal does not
// reveal channel identity, and the notice already says what the user needs.
type portalAccessFreezeView struct {
	Provider     string    `json:"provider"`
	ProviderName string    `json:"provider_name"`
	ReasonCode   string    `json:"reason_code"`
	Model        string    `json:"model"`
	FrozenAt     time.Time `json:"frozen_at"`
	Notice       string    `json:"notice"`
}

// GetPortalAccessFreezes lists the signed-in portal user's standing freezes.
func (h *Handler) GetPortalAccessFreezes(c *gin.Context) {
	user, ok := h.portalKeyAccess(c)
	if !ok {
		return
	}
	items := make([]portalAccessFreezeView, 0)
	service := accessfreeze.Runtime()
	if service != nil {
		subject := accessfreeze.Subject{TenantID: user.TenantID, Kind: accessfreeze.SubjectEndUser, ID: user.ID}
		for _, f := range service.Standing(subject) {
			name, known := accessfreeze.ProviderDisplayName(f.Provider)
			provider := f.Provider
			if !known {
				// An operator-named upstream: its name is the channel's.
				provider = ""
			}
			items = append(items, portalAccessFreezeView{
				Provider: provider, ProviderName: name, ReasonCode: f.ReasonCode, Model: f.Model,
				FrozenAt: f.FrozenAt, Notice: service.NoticeOf(f),
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

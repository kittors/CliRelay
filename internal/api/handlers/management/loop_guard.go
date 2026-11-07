package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/loopguard"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/loopdetect"
	log "github.com/sirupsen/logrus"
)

// The loop protection settings of a tenant and of its API keys. Both checks are
// off until a tenant turns them on, so these endpoints only ever opt a tenant
// in; nothing here changes how a tenant that never calls them is served.
//
//	GET/PUT /loop-guard           the tenant's own switches and thresholds
//	GET/PUT /loop-guard/keys/:id  one API key's override of them
//
// A PUT replaces the sections its body carries ("reply", "tool") and leaves the
// others as they are, so a client that knows only one of the checks cannot
// clear the other.

// loopGuardRegistry is the registry the endpoints read and write: the one the
// inference path resolves settings from, unless a test supplies its own.
func (h *Handler) loopGuardRegistry() *loopguard.Registry {
	if h != nil && h.loopGuard != nil {
		return h.loopGuard
	}
	return loopguard.Default()
}

// loopGuardWrite is the body of both PUTs. A section that is absent is left
// unchanged; one that is present replaces the stored section, so a threshold it
// does not name goes back to the default.
type loopGuardWrite struct {
	Reply *loopguard.ReplyRule `json:"reply"`
	Tool  *loopguard.ToolRule  `json:"tool"`
}

// scope is the part of the body that is present, for validation.
func (w loopGuardWrite) scope() loopguard.Scope {
	var s loopguard.Scope
	if w.Reply != nil {
		s.Reply = *w.Reply
	}
	if w.Tool != nil {
		s.Tool = *w.Tool
	}
	return s
}

// The views below are what the panel reads. They spell out every field, the
// unset ones included (a key's "enabled": null is "follow the tenant"), which
// the stored form leaves out.
type loopGuardTenantReply struct {
	Enabled     bool `json:"enabled"`
	Window      int  `json:"window"`
	MaxDistinct int  `json:"max_distinct"`
}

type loopGuardTenantTool struct {
	Enabled bool `json:"enabled"`
	Repeats int  `json:"repeats"`
}

type loopGuardKeyReply struct {
	Enabled     *bool `json:"enabled"`
	Window      int   `json:"window"`
	MaxDistinct int   `json:"max_distinct"`
}

type loopGuardKeyTool struct {
	Enabled *bool `json:"enabled"`
	Repeats int   `json:"repeats"`
}

type loopGuardEffectiveReply struct {
	Enabled bool `json:"enabled"`
	// Source is the level that switched the check on, "tenant" or "api_key".
	Source      string `json:"source,omitempty"`
	Window      int    `json:"window,omitempty"`
	MaxDistinct int    `json:"max_distinct,omitempty"`
}

type loopGuardEffectiveTool struct {
	Enabled bool   `json:"enabled"`
	Source  string `json:"source,omitempty"`
	Repeats int    `json:"repeats,omitempty"`
}

// loopGuardInfo is sent with every settings response, so a panel builds its
// inputs from what this backend enforces instead of from copies of the numbers.
type loopGuardInfo struct {
	// Persistent is false when no settings store is attached; the settings can
	// then be read but not saved.
	Persistent bool              `json:"persistent"`
	UpdatedAt  *time.Time        `json:"updated_at,omitempty"`
	Defaults   loopGuardDefaults `json:"defaults"`
	Bounds     loopGuardBounds   `json:"bounds"`
}

type loopGuardDefaults struct {
	ReplyWindow      int `json:"reply_window"`
	ReplyMaxDistinct int `json:"reply_max_distinct"`
	ToolRepeats      int `json:"tool_repeats"`
}

// loopGuardBounds are the limits a write is validated against. A threshold left
// at 0 follows the default.
type loopGuardBounds struct {
	MinReplyWindow   int `json:"min_reply_window"`
	MaxReplyWindow   int `json:"max_reply_window"`
	MinReplyDistinct int `json:"min_reply_distinct"`
	// ReplyDistinctWindowShare: max_distinct may be at most a window's units
	// divided by this.
	ReplyDistinctWindowShare int `json:"reply_distinct_window_share"`
	MinToolRepeats           int `json:"min_tool_repeats"`
	MaxToolRepeats           int `json:"max_tool_repeats"`
}

type loopGuardTenantResponse struct {
	Reply loopGuardTenantReply `json:"reply"`
	Tool  loopGuardTenantTool  `json:"tool"`
	// KeyOverrides is how many API keys carry an override of their own.
	KeyOverrides int `json:"key_overrides"`
	loopGuardInfo
}

type loopGuardKeyResponse struct {
	KeyID string            `json:"key_id"`
	Reply loopGuardKeyReply `json:"reply"`
	Tool  loopGuardKeyTool  `json:"tool"`
	// Tenant is what the key follows where it sets nothing.
	Tenant loopGuardTenantSettings `json:"tenant"`
	// Effective is what the gateway does for this key's requests.
	Effective loopGuardEffective `json:"effective"`
	loopGuardInfo
}

type loopGuardTenantSettings struct {
	Reply loopGuardTenantReply `json:"reply"`
	Tool  loopGuardTenantTool  `json:"tool"`
}

type loopGuardEffective struct {
	Reply loopGuardEffectiveReply `json:"reply"`
	Tool  loopGuardEffectiveTool  `json:"tool"`
}

func (h *Handler) loopGuardInfoFor(settings loopguard.Settings) loopGuardInfo {
	info := loopGuardInfo{
		Persistent: h.loopGuardRegistry().Persistent(),
		Defaults: loopGuardDefaults{
			ReplyWindow:      loopdetect.DefaultReplyWindow,
			ReplyMaxDistinct: loopdetect.DefaultReplyDistinct,
			ToolRepeats:      loopdetect.DefaultToolRepeats,
		},
		Bounds: loopGuardBounds{
			MinReplyWindow:           loopdetect.MinReplyWindow,
			MaxReplyWindow:           loopdetect.MaxReplyWindow,
			MinReplyDistinct:         loopdetect.MinReplyDistinct,
			ReplyDistinctWindowShare: loopdetect.MaxDistinctShare,
			MinToolRepeats:           loopdetect.MinToolRepeats,
			MaxToolRepeats:           loopdetect.MaxToolRepeats,
		},
	}
	if !settings.UpdatedAt.IsZero() {
		updated := settings.UpdatedAt
		info.UpdatedAt = &updated
	}
	return info
}

func tenantSettingsView(s loopguard.Settings) loopGuardTenantSettings {
	return loopGuardTenantSettings{
		Reply: loopGuardTenantReply{
			Enabled:     s.Reply.Enabled != nil && *s.Reply.Enabled,
			Window:      s.Reply.Window,
			MaxDistinct: s.Reply.MaxDistinct,
		},
		Tool: loopGuardTenantTool{
			Enabled: s.Tool.Enabled != nil && *s.Tool.Enabled,
			Repeats: s.Tool.Repeats,
		},
	}
}

func (h *Handler) loopGuardTenantView(s loopguard.Settings) loopGuardTenantResponse {
	view := tenantSettingsView(s)
	return loopGuardTenantResponse{
		Reply:         view.Reply,
		Tool:          view.Tool,
		KeyOverrides:  len(s.Keys),
		loopGuardInfo: h.loopGuardInfoFor(s),
	}
}

func (h *Handler) loopGuardKeyView(keyID string, s loopguard.Settings) loopGuardKeyResponse {
	override := s.Keys[keyID]
	effective := s.Effective(keyID)
	view := loopGuardKeyResponse{
		KeyID: keyID,
		Reply: loopGuardKeyReply{
			Enabled:     override.Reply.Enabled,
			Window:      override.Reply.Window,
			MaxDistinct: override.Reply.MaxDistinct,
		},
		Tool: loopGuardKeyTool{
			Enabled: override.Tool.Enabled,
			Repeats: override.Tool.Repeats,
		},
		Tenant:        tenantSettingsView(s),
		loopGuardInfo: h.loopGuardInfoFor(s),
	}
	if effective.Reply != nil {
		view.Effective.Reply = loopGuardEffectiveReply{
			Enabled:     true,
			Source:      string(effective.ReplySource),
			Window:      effective.Reply.Window,
			MaxDistinct: effective.Reply.Distinct,
		}
	}
	if effective.Tool != nil {
		view.Effective.Tool = loopGuardEffectiveTool{
			Enabled: true,
			Source:  string(effective.ToolSource),
			Repeats: effective.Tool.Repeats,
		}
	}
	return view
}

func loopGuardError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// loopGuardWriteError answers a failed write. The settings a write could not
// save have not changed, so every one of these is safe to retry.
func loopGuardWriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, loopguard.ErrNotPersistent):
		loopGuardError(c, http.StatusServiceUnavailable, "loop_guard_not_persistent", err.Error())
	case errors.Is(err, loopguard.ErrConflict):
		loopGuardError(c, http.StatusConflict, "loop_guard_conflict", "the loop protection settings were changed by someone else while saving; reload and try again")
	default:
		log.WithError(err).Warn("loop guard: saving settings failed")
		loopGuardError(c, http.StatusInternalServerError, "loop_guard_failed", "saving the loop protection settings failed")
	}
}

func bindLoopGuardWrite(c *gin.Context) (loopGuardWrite, bool) {
	var body loopGuardWrite
	if err := c.ShouldBindJSON(&body); err != nil {
		loopGuardError(c, http.StatusBadRequest, "invalid_request", "invalid body")
		return body, false
	}
	if err := body.scope().Validate(); err != nil {
		loopGuardError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return body, false
	}
	return body, true
}

// GetLoopGuard returns the loop protection settings of the tenant the session
// acts in.
func (h *Handler) GetLoopGuard(c *gin.Context) {
	settings := h.loopGuardRegistry().For(effectiveTenantID(c))
	c.JSON(http.StatusOK, h.loopGuardTenantView(settings))
}

// PutLoopGuard saves the tenant's own switches and thresholds. The overrides of
// its API keys are kept, except those of keys that no longer exist.
func (h *Handler) PutLoopGuard(c *gin.Context) {
	body, ok := bindLoopGuardWrite(c)
	if !ok {
		return
	}
	tenantID := effectiveTenantID(c)
	saved, err := h.loopGuardRegistry().Update(c.Request.Context(), tenantID, func(s *loopguard.Settings) error {
		if body.Reply != nil {
			s.Reply = *body.Reply
		}
		if body.Tool != nil {
			s.Tool = *body.Tool
		}
		dropOverridesOfDeletedKeys(s, tenantID)
		return nil
	})
	if err != nil {
		loopGuardWriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.loopGuardTenantView(saved))
}

// dropOverridesOfDeletedKeys removes the overrides whose API key is gone, which
// nothing else removes: deleting a key does not know about this setting. It runs
// inside the update, so a key created while it ran is seen on the retry a
// concurrent write forces.
func dropOverridesOfDeletedKeys(s *loopguard.Settings, tenantID string) {
	if len(s.Keys) == 0 {
		return
	}
	rows := usage.ListAPIKeysForTenant(tenantID)
	if len(rows) == 0 {
		// The list does not say an empty tenant from a store that could not be
		// read, and dropping every override on a failed read would be worse than
		// keeping the overrides of keys that are gone.
		return
	}
	alive := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		alive[strings.TrimSpace(row.ID)] = struct{}{}
	}
	for id := range s.Keys {
		if _, ok := alive[id]; !ok {
			delete(s.Keys, id)
		}
	}
}

// keyOfTenant resolves the :id of a key route to an API key of the tenant, and
// answers 404 itself when there is none: an id of another tenant's key is not
// found either.
func keyOfTenant(c *gin.Context) (string, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" || usage.GetAPIKeyByIDForTenant(effectiveTenantID(c), id) == nil {
		loopGuardError(c, http.StatusNotFound, "api_key_not_found", "api key not found")
		return "", false
	}
	return id, true
}

// GetLoopGuardKey returns one API key's override and what it comes to for that
// key's requests.
func (h *Handler) GetLoopGuardKey(c *gin.Context) {
	id, ok := keyOfTenant(c)
	if !ok {
		return
	}
	settings := h.loopGuardRegistry().For(effectiveTenantID(c))
	c.JSON(http.StatusOK, h.loopGuardKeyView(id, settings))
}

// PutLoopGuardKey saves one API key's override. A key whose override ends up
// setting nothing has none stored, and follows its tenant again.
func (h *Handler) PutLoopGuardKey(c *gin.Context) {
	id, ok := keyOfTenant(c)
	if !ok {
		return
	}
	body, ok := bindLoopGuardWrite(c)
	if !ok {
		return
	}
	saved, err := h.loopGuardRegistry().Update(c.Request.Context(), effectiveTenantID(c), func(s *loopguard.Settings) error {
		override := s.Keys[id]
		if body.Reply != nil {
			override.Reply = *body.Reply
		}
		if body.Tool != nil {
			override.Tool = *body.Tool
		}
		if s.Keys == nil {
			s.Keys = make(map[string]loopguard.Scope, 1)
		}
		s.Keys[id] = override
		return nil
	})
	if err != nil {
		loopGuardWriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, h.loopGuardKeyView(id, saved))
}

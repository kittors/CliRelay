package management

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/authevents"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/enduser"
)

// portalAttemptWindow bounds what an end user can look back over. They are
// answering "was this me?", which is a recent-history question; a longer window
// only widens what one compromised portal session can enumerate.
const portalAttemptWindow = 30 * 24 * time.Hour

// maxPortalAttemptSize caps one page for the portal.
const maxPortalAttemptSize = 50

// GetPortalAuthAttempts returns the caller's own sign-in history.
//
// The customer-facing half of the attempt log: when an end user asks "why is my
// quota gone" or "was that login me", the answer should not require them to
// open a support ticket and an operator to run a query.
//
// The username filter is taken from the authenticated session and never from the
// request, so a portal session cannot read anyone else's history.
func (h *Handler) GetPortalAuthAttempts(c *gin.Context) {
	user, _, ok := h.authenticatePortal(c)
	if !ok {
		return
	}
	recorder := authevents.Default()
	if recorder == nil || !recorder.Available() {
		authEventsUnavailable(c)
		return
	}

	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if size < 1 || size > maxPortalAttemptSize {
		size = 20
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}

	events, total, err := recorder.List(c.Request.Context(), authevents.ListFilter{
		Username: enduser.NormalizeUsername(user.Username),
		Surface:  authevents.SurfacePortalLogin,
		Since:    time.Now().Add(-portalAttemptWindow),
		Page:     page,
		Size:     size,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "list_failed", "message": err.Error()}})
		return
	}

	// Deliberately reduced compared with the operator view: an end user needs to
	// recognise their own sessions, not to receive a reconnaissance feed. Scope,
	// request id and the trust flag are operator diagnostics and stay out.
	items := make([]gin.H, 0, len(events))
	for i := range events {
		items = append(items, gin.H{
			"occurred_at": events[i].OccurredAt,
			"ip":          maskPortalIP(events[i].IP),
			"outcome":     events[i].Outcome,
			"user_agent":  events[i].UserAgent,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "page": page, "size": size})
}

// maskPortalIP hides the final octet of an address shown to an end user.
//
// Enough to recognise "that was my office" or "that was not me", without handing
// a full address to whoever is holding a stolen portal session.
func maskPortalIP(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, ":") {
		// IPv6: keep the routing prefix, drop the interface half.
		parts := strings.Split(trimmed, ":")
		if len(parts) > 4 {
			return strings.Join(parts[:4], ":") + ":···"
		}
		return trimmed
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) != 4 {
		return trimmed
	}
	return strings.Join(parts[:3], ".") + ".×"
}

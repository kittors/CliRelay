package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// PortalAuthThrottleMiddleware rate-limits the end-user portal's login and
// refresh endpoints.
//
// Those routes deliberately live outside Handler.Middleware() — they must be
// reachable without a management key — which until now left them with no
// throttling whatsoever: PostPortalLogin runs an unconditional bcrypt compare
// (cost 10) for every request, including ones naming a user that does not
// exist, so a few dozen concurrent connections saturate a core. It is exported
// because the throttle has to be attached where the routes are registered.
//
// The check runs before c.Next() so a throttled caller never reaches the hash
// comparison; charging happens after, and only for 401, so that a cooldown or a
// disabled account (403/423) does not consume the guess budget.
func (h *Handler) PortalAuthThrottleMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := h.clientThrottleKey(c, scopePortalPassword)
		if d := h.loginThrottle.evaluate(key, time.Now()); d.Outcome != outcomeAllow {
			abortThrottled(c, d)
			return
		}
		c.Next()
		if c.Writer.Status() == http.StatusUnauthorized {
			d := h.loginThrottle.recordFailure(key, time.Now())
			h.logAuthFailure(c, key, d)
		}
	}
}

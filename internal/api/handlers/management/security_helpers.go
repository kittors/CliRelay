package management

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func retryAfterSecondsHeader(duration time.Duration) string {
	return strconv.Itoa(retryAfterSeconds(duration))
}

// retryAfterSeconds renders a lock's remaining time as whole seconds. It rounds
// up, so a client that waits exactly that long never arrives a moment early, and
// never goes below 1, so a live lock never reads as "retry in 0 seconds".
func retryAfterSeconds(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

func shouldReadManagementTokenFromQuery(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	if strings.TrimSpace(c.Query("token")) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.GetHeader("Upgrade")), "websocket") {
		return false
	}
	path := strings.TrimSpace(c.FullPath())
	if path == "" {
		path = c.Request.URL.Path
	}
	// Query-string credentials are kept only for browser WebSocket handshakes,
	// where custom Authorization headers cannot be set.
	return path == "/v0/management/system-stats/ws" || strings.HasSuffix(path, "/system-stats/ws")
}

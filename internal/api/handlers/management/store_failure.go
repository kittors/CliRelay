package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/dberr"
	log "github.com/sirupsen/logrus"
)

// storeRetryAfterSeconds is the Retry-After hint sent while the store cannot be
// reached. The panel's token refresher waits this long between its two
// attempts, so it stays well inside that refresher's 17-second budget.
const storeRetryAfterSeconds = "2"

// abortStoreFailure answers an error that is not a decision about the caller,
// its credentials or its input: the identity or end-user store could not
// answer the question.
//
// A store that cannot be reached answers 503 identity_unavailable. It must
// never look like a credential decision: the panel treats a 401 from
// /v0/auth/refresh as a dead session and signs the admin out, which is what a
// one-minute loss of the link to the database did on 2026-11-20. The error
// itself is logged rather than echoed, because a connect error names the
// database host and role, and /v0/auth/login and /v0/auth/refresh answer
// callers that are not signed in.
//
// Any other unexpected error keeps the existing 500 internal_error answer with
// its message, which the panel shows to the person who triggered it.
func abortStoreFailure(c *gin.Context, err error) {
	if dberr.IsTransient(err) {
		log.WithError(err).Warnf("management: store unavailable for %s %s", c.Request.Method, c.FullPath())
		c.Header("Retry-After", storeRetryAfterSeconds)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"code": "identity_unavailable", "message": "identity store temporarily unavailable",
		}})
		return
	}
	log.WithError(err).Errorf("management: unexpected store error for %s %s", c.Request.Method, c.FullPath())
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": err.Error()}})
}

package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
)

// respondSharedAuthStatus answers get-auth-status from the shared session
// store in cluster mode and reports whether it did.
//
// A single node answers an unknown state with "ok", and the panel treats "ok"
// as a finished login. There that answer is how a login superseded by another
// completed login of the same provider reports back, since the memory store
// deletes superseded sessions; it is also what old panels expect. In a
// cluster an unknown state is more often a node that never saw the login, or
// one that lost it in a restart, so saying "ok" would announce logins that
// never happened. Here an unknown or expired state is reported as such, while
// a superseded one keeps answering "ok" as it does on a single node.
func respondSharedAuthStatus(c *gin.Context, state string) bool {
	shared := sharedOAuthSessions.Load()
	if shared == nil {
		return false
	}
	lookup, err := shared.Lookup(state)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "oauth session store unavailable"})
		return true
	}
	if lookup.Outcome != oauthsession.LookupMissing {
		if tenantID := lookup.Session.TenantID; tenantID != "" && tenantID != effectiveTenantID(c) {
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
			return true
		}
	}
	switch lookup.Outcome {
	case oauthsession.LookupMissing:
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "oauth state not found or expired"})
	case oauthsession.LookupExpired:
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": oauthsession.MessageExpired})
	case oauthsession.LookupSuperseded:
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	default:
		switch status := lookup.Session.Status; {
		case status == oauthSessionStatusCompleted:
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		case status != "":
			c.JSON(http.StatusOK, gin.H{"status": "error", "error": status})
		default:
			c.JSON(http.StatusOK, gin.H{"status": "wait"})
		}
	}
	return true
}

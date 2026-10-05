package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
)

// respondSharedAuthStatus answers get-auth-status from the shared session
// store in cluster mode and reports whether it did.
//
// The panel treats "ok" as a finished login. A login superseded by another
// completed login of the same provider answers "ok" (the provider is signed
// in); an unknown or expired state is reported as such, since saying "ok"
// would announce a login that never happened. The single-node path in
// GetAuthStatus follows the same rules now that the memory store keeps
// superseded sessions as tombstones instead of deleting them.
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
			c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state", "code": oauthCodeSessionExpired})
			return true
		}
	}
	switch lookup.Outcome {
	case oauthsession.LookupMissing:
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "oauth state not found or expired", "code": oauthCodeSessionExpired})
	case oauthsession.LookupExpired:
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": oauthsession.MessageExpired, "code": oauthCodeSessionExpired})
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

// sharedOAuthStateSuperseded reports whether, in cluster mode, a state the
// session store no longer serves was cancelled because another login of the
// same provider completed. The memory store keeps such logins as tombstones
// that Get returns; the shared store only tells them apart through Lookup.
// Another tenant's state is never reported, so the answer reveals nothing a
// plain "unknown state" would not.
func sharedOAuthStateSuperseded(c *gin.Context, state string) bool {
	shared := sharedOAuthSessions.Load()
	if shared == nil {
		return false
	}
	lookup, err := shared.Lookup(state)
	if err != nil || lookup.Outcome != oauthsession.LookupSuperseded {
		return false
	}
	tenantID := lookup.Session.TenantID
	return tenantID == "" || tenantID == effectiveTenantID(c)
}

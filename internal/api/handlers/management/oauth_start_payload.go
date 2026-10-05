package management

import (
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
)

// How a started login finishes, reported to the panel as "flow".
//
// The panel used to get only url/state and showed the same "paste the callback
// URL" box for every provider, which is what left operators asking where that
// URL comes from. With the flow it can guide each kind of login:
//   - redirect: the browser lands on a localhost callback. On a remote server
//     that page cannot open; its full address is what gets pasted back.
//   - code: the provider's own page shows an authorization code to paste back
//     (Claude's platform callback).
//   - device: nothing to paste; the login finishes once the operator approves
//     the user code on the verification page.
const (
	oauthFlowRedirect = "redirect"
	oauthFlowCode     = "code"
	oauthFlowDevice   = "device"
)

// oauthStartPayload is the response of the *-auth-url start endpoints. It keeps
// the fields older panels read (status, url, state) and adds the flow and the
// moment the login session lapses, so a panel can count down instead of letting
// a stale link fail later. extra carries flow-specific fields.
func oauthStartPayload(flow, authURL, state string, expiresAt time.Time, extra gin.H) gin.H {
	payload := gin.H{
		"status":     "ok",
		"url":        authURL,
		"state":      state,
		"flow":       flow,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	}
	for key, value := range extra {
		payload[key] = value
	}
	return payload
}

// oauthSessionExpiry is when a login started now stops accepting its callback.
func oauthSessionExpiry(now time.Time) time.Time {
	return now.Add(oauthSessionTTL)
}

// deviceLoginExpiry caps the session expiry by the device code's own lifetime,
// which can be shorter than the session TTL.
func deviceLoginExpiry(now time.Time, expiresInSeconds int) time.Time {
	expiry := oauthSessionExpiry(now)
	if expiresInSeconds > 0 {
		if deviceExpiry := now.Add(time.Duration(expiresInSeconds) * time.Second); deviceExpiry.Before(expiry) {
			return deviceExpiry
		}
	}
	return expiry
}

// anthropicLoginFlow reports whether a Claude login redirects to the localhost
// forwarder or to Anthropic's code page. The code page is used when the panel
// asked for it and also as the fallback when no forwarder could start, so the
// answer is read from the authorization URL that was actually issued.
func anthropicLoginFlow(authURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(authURL))
	if err != nil {
		return oauthFlowRedirect
	}
	if parsed.Query().Get("redirect_uri") == internalclaude.PlatformRedirectURI {
		return oauthFlowCode
	}
	return oauthFlowRedirect
}

// deviceLoginExtra is the device-flow part of a start payload.
func deviceLoginExtra(userCode, verificationURI string, expiresInSeconds int) gin.H {
	extra := gin.H{}
	if code := strings.TrimSpace(userCode); code != "" {
		extra["user_code"] = code
	}
	if uri := strings.TrimSpace(verificationURI); uri != "" {
		extra["verification_uri"] = uri
	}
	if expiresInSeconds > 0 {
		extra["expires_in"] = expiresInSeconds
	}
	return extra
}

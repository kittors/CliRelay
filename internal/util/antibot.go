package util

import (
	"net/http"
	"strings"
)

// IsAntiBotChallenge reports whether a response is an anti-bot interstitial
// (Cloudflare's "Just a moment…" page and the like) rather than an answer from
// the service itself. Such a refusal says nothing about the credential that was
// sent, so callers report it separately instead of calling the credential bad.
func IsAntiBotChallenge(header http.Header, body []byte) bool {
	if header != nil && strings.EqualFold(strings.TrimSpace(header.Get("cf-mitigated")), "challenge") {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(string(body)))
	if !strings.HasPrefix(text, "<") {
		return false
	}
	for _, marker := range []string{"just a moment", "cf-chl", "challenge-platform", "attention required", "cf_chl_opt"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

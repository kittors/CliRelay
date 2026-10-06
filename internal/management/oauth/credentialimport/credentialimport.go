// Package credentialimport holds what the credential-import flows share: the
// kinds of failure the panel reacts to, and the facts reported back about an
// imported account.
//
// An import turns a credential the operator already has (a claude.ai session
// cookie, an OpenAI or Google refresh token, a Grok web SSO cookie) into the
// same account a browser login would have produced. Each provider wraps its
// failures in one of the sentinels below so the management API can answer with
// a machine-readable code instead of the upstream's raw message.
package credentialimport

import (
	"errors"
	"strings"
)

var (
	// ErrInvalidCredential: the upstream rejected the credential (expired,
	// revoked, malformed, or belonging to nobody).
	ErrInvalidCredential = errors.New("credential is invalid or expired")
	// ErrBlocked: the upstream refused the request itself, typically a
	// Cloudflare challenge, before looking at the credential.
	ErrBlocked = errors.New("upstream blocked the request")
	// ErrNoOrganization: the account exists but has no organization to
	// authorize, so there is nothing to sign in to.
	ErrNoOrganization = errors.New("account has no organization")
	// ErrDenied: the upstream declined to authorize the client for this
	// account.
	ErrDenied = errors.New("authorization was denied")
)

// Details are the facts worth showing about an imported account.
type Details struct {
	Email        string
	Organization string
	Plan         string
}

// maxMessageLength bounds what an upstream error contributes to a response.
const maxMessageLength = 300

// SafeMessage renders err for an API response: every secret is masked (an
// upstream may echo what it was sent), whitespace is collapsed and the result
// is bounded. Call it with every credential value the import handled.
func SafeMessage(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if len(secret) < 6 {
			continue
		}
		message = strings.ReplaceAll(message, secret, "***")
	}
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > maxMessageLength {
		message = message[:maxMessageLength] + "…"
	}
	return message
}

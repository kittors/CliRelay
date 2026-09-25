package management

import (
	"sync/atomic"
	"time"

	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
)

const (
	oauthSessionTTL             = oauthsession.DefaultTTL
	maxOAuthStateLength         = oauthsession.MaxStateLength
	oauthSessionStatusCompleted = oauthsession.StatusCompleted
)

var (
	errInvalidOAuthState      = oauthsession.ErrInvalidState
	errUnsupportedOAuthFlow   = oauthsession.ErrUnsupportedFlow
	errOAuthSessionNotPending = oauthsession.ErrNotPending
)

var oauthSessions = newOAuthSessionStore(oauthSessionTTL)

// sharedOAuthSessions replaces oauthSessions in cluster mode, where the
// callback and the status polls of one login may reach different nodes.
var sharedOAuthSessions atomic.Pointer[oauthsession.ClusterStore]

func newOAuthSessionStore(ttl time.Duration) *oauthsession.Store {
	return oauthsession.NewStore(ttl)
}

// SetSharedOAuthSessions makes every OAuth login of this process use store;
// nil restores the in-process store.
func SetSharedOAuthSessions(store *oauthsession.ClusterStore) {
	sharedOAuthSessions.Store(store)
}

func activeOAuthSessions() oauthsession.Backend {
	if shared := sharedOAuthSessions.Load(); shared != nil {
		return shared
	}
	return oauthSessions
}

func RegisterOAuthSession(state, provider string) {
	activeOAuthSessions().RegisterTenant(state, provider, "")
}

func RegisterOAuthSessionForTenant(state, provider, tenantID string) {
	activeOAuthSessions().RegisterTenant(state, provider, tenantID)
}

func SetOAuthSessionError(state, message string) { activeOAuthSessions().SetError(state, message) }

func CompleteOAuthSession(state string) { activeOAuthSessions().Complete(state) }

func CompleteOAuthSessionsByProvider(provider string) int {
	return activeOAuthSessions().CompleteProviderTenant(provider, "")
}

func CompleteOAuthSessionsByProviderForTenant(provider, tenantID string) int {
	return activeOAuthSessions().CompleteProviderTenant(provider, tenantID)
}

func GetOAuthSession(state string) (provider string, status string, ok bool) {
	session, ok := activeOAuthSessions().Get(state)
	if !ok {
		return "", "", false
	}
	return session.Provider, session.Status, true
}

func GetOAuthSessionWithTenant(state string) (provider, tenantID, status string, ok bool) {
	session, ok := activeOAuthSessions().Get(state)
	if !ok {
		return "", "", "", false
	}
	return session.Provider, session.TenantID, session.Status, true
}

func IsOAuthSessionPending(state, provider string) bool {
	return activeOAuthSessions().IsPending(state, provider)
}

func ValidateOAuthState(state string) error {
	return oauthsession.ValidateState(state)
}

func NormalizeOAuthProvider(provider string) (string, error) {
	return oauthsession.NormalizeProvider(provider)
}

func WriteOAuthCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	return oauthsession.WriteCallbackFile(authDir, provider, state, code, errorMessage)
}

// WriteOAuthCallbackFileForPendingSession hands a callback to the pending
// session state. In cluster mode it lands on the shared session row and no
// file is written, so the returned path is empty.
func WriteOAuthCallbackFileForPendingSession(authDir, provider, state, code, errorMessage string) (string, error) {
	if shared := sharedOAuthSessions.Load(); shared != nil {
		return "", shared.DeliverCallback(authDir, provider, state, code, errorMessage)
	}
	if _, tenantID, _, ok := GetOAuthSessionWithTenant(state); ok && tenantID != "" {
		authDir = managementauthfiles.TenantAuthDir(authDir, tenantID)
	}
	return oauthSessions.WriteCallbackFileForPending(authDir, provider, state, code, errorMessage)
}

func WaitOAuthCallbackFile(authDir, provider, state string, timeout time.Duration) (map[string]string, error) {
	return activeOAuthSessions().WaitCallback(authDir, provider, state, timeout, 500*time.Millisecond)
}

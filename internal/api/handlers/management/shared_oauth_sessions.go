package management

import (
	"sync/atomic"

	oauthsession "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/session"
)

type sharedOAuthSessionHolder struct {
	store oauthsession.Backend
}

// sharedOAuthSessions is configured by cluster startup. A nil store keeps the
// single-node in-memory OAuth behavior unchanged.
var sharedOAuthSessions atomic.Pointer[sharedOAuthSessionHolder]

// SetSharedOAuthSessions makes OAuth session registration, callback delivery,
// and status polling use the cluster-backed store.
func SetSharedOAuthSessions(store oauthsession.Backend) {
	if store == nil {
		sharedOAuthSessions.Store(nil)
		return
	}
	sharedOAuthSessions.Store(&sharedOAuthSessionHolder{store: store})
}

func sharedOAuthSessionStore() oauthsession.Backend {
	if holder := sharedOAuthSessions.Load(); holder != nil {
		return holder.store
	}
	return nil
}

// sharedOAuthSessionLookup is implemented by ClusterStore to distinguish an
// expired or superseded row from a state that never existed.
type sharedOAuthSessionLookup interface {
	Lookup(string) (oauthsession.LookupResult, error)
}

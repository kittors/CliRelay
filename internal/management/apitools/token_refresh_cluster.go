package apitools

import (
	"context"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// refreshLeaseWait bounds how long a management call waits for another
// node's refresh of the same credential; that refresh usually finishes in a
// few seconds, and its result is then used instead of refreshing again.
var refreshLeaseWait = 20 * time.Second

type refreshBaselineKey struct{}

// coordinatedRefresh runs refresh, one of the provider refresh functions of
// this package, under the cluster refresh lease the background refresher
// uses too, so a rotating refresh token is spent by one node only. When
// another node refreshed while this call waited, its copy is adopted first
// and usually needs no refresh at all. Without a coordinator (single node),
// or while the current token is still fresh, refresh runs as before.
func (s *Service) coordinatedRefresh(ctx context.Context, auth *coreauth.Auth, needsRefresh func(map[string]any) bool, refresh func(context.Context, *coreauth.Auth) (string, error)) (string, error) {
	if s == nil || s.authManager == nil || auth == nil || !s.authManager.RefreshCoordinated() {
		return refresh(ctx, auth)
	}
	if strings.TrimSpace(TokenValueFromMetadata(auth.Metadata)) != "" && !needsRefresh(auth.Metadata) {
		return refresh(ctx, auth)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lease, err := s.authManager.AcquireRefreshLease(ctx, auth.ID, refreshLeaseWait)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	if latest := lease.Latest; latest != nil && coreauth.CredentialVersion(latest) > coreauth.CredentialVersion(auth) {
		*auth = *s.authManager.AdoptPersistedCredential(ctx, latest)
	}
	return refresh(context.WithValue(ctx, refreshBaselineKey{}, coreauth.SnapshotMetadata(auth.Metadata)), auth)
}

// persistRefreshedAuth saves a refreshed credential. Under the cluster lease a
// write that lost a version race re-applies the refreshed keys onto the
// newest copy instead of dropping the rotated tokens; on a single node it is a
// plain update.
func (s *Service) persistRefreshedAuth(ctx context.Context, auth *coreauth.Auth) {
	before, _ := ctx.Value(refreshBaselineKey{}).(coreauth.MetadataSnapshot)
	_, _ = s.authManager.UpdateRefreshed(ctx, auth, before)
}

// kimiTokenNeedsRefresh reports whether a Kimi access token is missing an
// expiry or expires within 30 seconds.
func kimiTokenNeedsRefresh(metadata map[string]any) bool {
	expStr := stringValue(metadata, "expired")
	if expStr == "" {
		return true
	}
	ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr))
	if errParse != nil {
		return true
	}
	return !time.Now().Add(30 * time.Second).Before(ts)
}

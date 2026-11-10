package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/antigravity"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

// Every generation request names the account's own Cloud Code project. A
// credential that lacks one gets it discovered before the request is sent;
// nothing invents a name any more.
//
// The executor used to make one up (`useful-fuze-1a2b3`) whenever the
// credential had none. Paid accounts were served anyway, but a free-tier
// account answered 403 SUBSCRIPTION_REQUIRED (#3501), which read as a missing
// licence when the account only lacked its project (#1032). Measured on
// production before this change: every credential stored without a project
// got one back from loadCodeAssist once asked with the right client identity.

const (
	// antigravityDiscoveryTimeout bounds one discovery, onboarding included.
	// It runs detached from whichever request started it, so a client that
	// gives up does not cut it short for the others waiting on it.
	antigravityDiscoveryTimeout = 90 * time.Second
	// antigravityRequestDiscoveryWait is how long a request waits for a
	// discovery before giving the account up for this attempt. The discovery
	// keeps running and the next request finds its result.
	antigravityRequestDiscoveryWait = 20 * time.Second
	// antigravityRequestProbeBackoff is how long a failed discovery holds off
	// the next one started by a request. It is short because the account
	// cannot serve at all meanwhile. A "no project" verdict holds for
	// antigravityauth.ProjectIDUnavailableRetryAfter instead.
	antigravityRequestProbeBackoff = time.Minute
	// antigravityProjectEntryTTL drops registry entries nobody refreshed, such
	// as those of a credential that was deleted.
	antigravityProjectEntryTTL = 2 * antigravityauth.ProjectIDUnavailableRetryAfter
)

type antigravityProjectEntry struct {
	projectID string
	err       error
	retryAt   time.Time
	storedAt  time.Time
}

// antigravityProjectRegistry remembers, per credential, the project a request
// discovered and the failures it ran into, and runs one discovery at a time
// per credential. A request's copy of the credential is dropped once the
// request ends, so this is what carries a found project to the next request
// and to the background refresh, which writes it into the credential where it
// is persisted. It is package-level because a config reload rebuilds every
// executor.
type antigravityProjectRegistry struct {
	group   singleflight.Group
	mu      sync.Mutex
	entries map[string]antigravityProjectEntry
}

var antigravityProjects = newAntigravityProjectRegistry()

func newAntigravityProjectRegistry() *antigravityProjectRegistry {
	return &antigravityProjectRegistry{entries: make(map[string]antigravityProjectEntry)}
}

// antigravityProjectKey identifies a credential for the registry. A new login
// of the same account keeps the file name but brings a new refresh token, so
// the token is part of the key: a verdict about the old credential must not
// stick to the new one. Google keeps refresh tokens stable across refreshes.
func antigravityProjectKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	id := strings.TrimSpace(auth.ID)
	if id == "" {
		id = strings.TrimSpace(auth.FileName)
	}
	if id == "" {
		return ""
	}
	if refreshToken := metaStringValue(auth.Metadata, "refresh_token"); refreshToken != "" {
		sum := sha256.Sum256([]byte(refreshToken))
		return id + "#" + hex.EncodeToString(sum[:6])
	}
	return id
}

// known returns the project already discovered for auth, if any.
func (r *antigravityProjectRegistry) known(auth *cliproxyauth.Auth) string {
	key := antigravityProjectKey(auth)
	if key == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[key].projectID
}

// failure returns the error of a failed discovery for auth that still holds.
func (r *antigravityProjectRegistry) failure(auth *cliproxyauth.Auth, now time.Time) error {
	key := antigravityProjectKey(auth)
	if key == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok || entry.projectID != "" || entry.err == nil || !now.Before(entry.retryAt) {
		return nil
	}
	return entry.err
}

func (r *antigravityProjectRegistry) record(key, projectID string, err error, now time.Time) {
	entry := antigravityProjectEntry{projectID: projectID, err: err, storedAt: now}
	if err != nil {
		entry.retryAt = now.Add(antigravityRequestProbeBackoff)
		if antigravityauth.IsTerminalProjectProbeError(err) {
			entry.retryAt = now.Add(antigravityauth.ProjectIDUnavailableRetryAfter)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for other, stale := range r.entries {
		if now.Sub(stale.storedAt) > antigravityProjectEntryTTL {
			delete(r.entries, other)
		}
	}
	r.entries[key] = entry
}

// discover runs fetch once for all concurrent callers asking about the same
// credential, remembers the outcome, and waits for it at most wait (0 waits
// until it is done) or until ctx ends.
func (r *antigravityProjectRegistry) discover(ctx context.Context, auth *cliproxyauth.Auth, wait time.Duration, fetch func(context.Context) (string, error)) (string, error) {
	key := antigravityProjectKey(auth)
	run := func() (any, error) {
		discoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), antigravityDiscoveryTimeout)
		defer cancel()
		projectID, err := fetch(discoveryCtx)
		projectID = strings.TrimSpace(projectID)
		if err == nil && projectID == "" {
			err = antigravityauth.ErrNoProjectID
		}
		if key != "" {
			r.record(key, projectID, err, time.Now())
		}
		return projectID, err
	}
	if key == "" {
		projectID, err := run()
		return projectID.(string), err
	}

	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case result := <-r.group.DoChan(key, run):
		projectID, _ := result.Val.(string)
		return projectID, result.Err
	case <-timeout:
		return "", errAntigravityDiscoveryPending
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var errAntigravityDiscoveryPending = errors.New("project discovery is still running")

// antigravityProjectIDFromAuth returns the project stored on the credential.
func antigravityProjectIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return metaStringValue(auth.Metadata, "project_id")
}

// missingAntigravityProjectError is what a request on a credential without a
// project fails with. It is a 503 so the manager moves on to another account
// and comes back shortly; the message says what is missing instead of passing
// the account off as unlicensed.
func missingAntigravityProjectError(cause error) error {
	msg := "antigravity account has no Cloud Code project yet"
	if cause != nil {
		msg += ": " + cause.Error()
	}
	return statusErr{code: http.StatusServiceUnavailable, msg: msg}
}

// withRequestProject returns auth carrying the project a generation request
// must name. A credential that has one is returned as it is; otherwise the
// project is discovered and set on a copy.
func (e *AntigravityExecutor) withRequestProject(ctx context.Context, auth *cliproxyauth.Auth, token string) (*cliproxyauth.Auth, error) {
	if antigravityProjectIDFromAuth(auth) != "" {
		return auth, nil
	}
	projectID, err := e.requestProjectID(ctx, auth, token)
	if err != nil {
		return nil, err
	}
	withProject := auth.Clone()
	if withProject.Metadata == nil {
		withProject.Metadata = make(map[string]any)
	}
	withProject.Metadata["project_id"] = projectID
	return withProject, nil
}

func (e *AntigravityExecutor) requestProjectID(ctx context.Context, auth *cliproxyauth.Auth, token string) (string, error) {
	if projectID := antigravityProjects.known(auth); projectID != "" {
		return projectID, nil
	}
	now := time.Now()
	if errFailed := antigravityProjects.failure(auth, now); errFailed != nil {
		return "", missingAntigravityProjectError(errFailed)
	}
	// The background refresh records its own outcome on the credential. A
	// "no project" verdict there holds for its whole window; after any other
	// failure a request may try again sooner than the refresh would.
	if auth != nil && antigravityauth.ShouldSkipProjectIDProbe(auth.Metadata, now, antigravityRequestProbeBackoff) {
		return "", missingAntigravityProjectError(storedProjectProbeError(auth.Metadata))
	}
	projectID, err := antigravityProjects.discover(ctx, auth, antigravityRequestDiscoveryWait, func(discoveryCtx context.Context) (string, error) {
		return e.fetchAntigravityProjectID(discoveryCtx, auth, token)
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return "", err
			}
		}
		return "", missingAntigravityProjectError(err)
	}
	return projectID, nil
}

// fetchAntigravityProjectID asks CloudCode for the account's project through
// the account's own egress.
func (e *AntigravityExecutor) fetchAntigravityProjectID(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) (string, error) {
	token := strings.TrimSpace(accessToken)
	if token == "" && auth != nil {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return "", errors.New("no access token to discover the project with")
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return sdkAuth.FetchAntigravityProjectID(ctx, token, httpClient)
}

func storedProjectProbeError(metadata map[string]any) error {
	if msg := metaStringValue(metadata, antigravityauth.MetaProjectIDProbeErr); msg != "" {
		return errors.New("last discovery: " + msg)
	}
	return errors.New("discovery is backing off")
}

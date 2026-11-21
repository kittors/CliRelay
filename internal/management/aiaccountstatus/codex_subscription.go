package aiaccountstatus

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// codexAccountsCheckURL answers with each ChatGPT workspace's billing
// entitlement: expires_at, renews_at, cancels_at, billing_period. Codex Desktop
// calls it with the Codex OAuth access token itself (to place its ChatGPT
// webview), so it is a request the provider already sees from Codex clients.
//
// Nothing else carries the cycle: Codex's own wham/accounts/check and
// wham/usage report the plan type only, and the id_token claims stop at the
// first billing cycle (see readProviderAssertedAccountFacts).
const codexAccountsCheckURL = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"

const (
	// A cycle moves once a month or year; one read a day keeps the dates
	// current without adding a request to every 15-minute quota round.
	codexSubscriptionRecheck = 24 * time.Hour
	// Once a renewal or end the last read knew about has passed, read again
	// within the hour so the new cycle, or the lapse, shows up the same day.
	codexSubscriptionAfterBoundary = time.Hour
	// A failed read waits this long, so an account the endpoint refuses is not
	// asked again on every quota round.
	codexSubscriptionFailureBackoff = 3 * time.Hour
)

// codexSubscriptionGET sends the accounts/check request. Tests replace it; the
// quota probe itself is replaced through Service.SetProbeFunc instead.
var codexSubscriptionGET = doAuthGET

// codexSubscriptionFailures remembers failed reads per account subject. It is
// process-local: after a restart, or on another node, an account may be asked
// once more, which costs one request.
var codexSubscriptionFailures = struct {
	sync.Mutex
	at map[string]time.Time
}{at: make(map[string]time.Time)}

// readCodexSubscriptionIfDue reads the account's billing cycle from the
// provider when the stored one is missing or due for a recheck. ok is false
// when nothing was read; the stored cycle then stays as it is.
func readCodexSubscriptionIfDue(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, now time.Time) (usage.AIAccountSubscription, bool) {
	identity := usage.ResolveAuthSubjectIdentity(auth)
	// Only an account with a stable provider id is read: the response is keyed
	// by that id, and the cycle is stored on the subject the id belongs to.
	if identity == nil || identity.ID == "" || !identity.ShareEligible || strings.TrimSpace(identity.AccountID) == "" {
		return usage.AIAccountSubscription{}, false
	}
	stored, readable := storedSubscription(identity.ID)
	if !readable || !codexSubscriptionDue(identity.ID, stored, now) {
		return usage.AIAccountSubscription{}, false
	}
	accountID := strings.TrimSpace(identity.AccountID)
	body, err := codexSubscriptionGET(ctx, svc, auth, codexAccountsCheckURL, codexProbeHeaders(), withCodexAccount(accountID))
	if err == nil {
		if subscription, ok := parseCodexEntitlement(body, accountID, now); ok {
			forgetCodexSubscriptionFailure(identity.ID)
			return subscription, true
		}
		err = errors.New("response has no entitlement for the account")
	}
	rememberCodexSubscriptionFailure(identity.ID, now)
	log.WithFields(log.Fields{"auth_subject_id": identity.ID, "auth_index": auth.Index}).
		WithError(err).Warn("codex subscription read failed; the stored billing cycle is kept")
	return usage.AIAccountSubscription{}, false
}

// storedSubscription returns the cycle on record for the subject. readable is
// false when the store could not be read: the round then skips the upstream
// read rather than taking "unknown" for "never read" and asking every round.
func storedSubscription(subjectID string) (usage.AIAccountSubscription, bool) {
	rows, err := usage.ListAIAccountSubjectStatus([]string{subjectID})
	if err != nil {
		return usage.AIAccountSubscription{}, false
	}
	if len(rows) == 0 {
		return usage.AIAccountSubscription{}, true
	}
	return rows[0].Subscription, true
}

// codexSubscriptionDue decides whether this probe round reads the billing
// cycle. Reads are a day apart, sooner only right after a renewal or end the
// last read knew about, and never sooner than the failure backoff.
func codexSubscriptionDue(subjectID string, stored usage.AIAccountSubscription, now time.Time) bool {
	codexSubscriptionFailures.Lock()
	failedAt, failed := codexSubscriptionFailures.at[subjectID]
	codexSubscriptionFailures.Unlock()
	if failed && now.Sub(failedAt) < codexSubscriptionFailureBackoff {
		return false
	}
	if stored.Source != usage.SubscriptionSourceProbe || stored.CheckedAt == nil {
		return true
	}
	since := now.Sub(*stored.CheckedAt)
	if since >= codexSubscriptionRecheck {
		return true
	}
	return since >= codexSubscriptionAfterBoundary && cycleBoundaryCrossed(stored, now)
}

// cycleBoundaryCrossed reports whether a renewal or end that was still ahead
// at the last read has passed since. One that had already passed then does not
// count, so a lapsed plan is not re-read every hour.
func cycleBoundaryCrossed(stored usage.AIAccountSubscription, now time.Time) bool {
	for _, at := range []*time.Time{stored.RenewsAt, stored.CancelsAt, stored.ExpiresAt} {
		if at != nil && at.After(*stored.CheckedAt) && !at.After(now) {
			return true
		}
	}
	return false
}

func rememberCodexSubscriptionFailure(subjectID string, at time.Time) {
	codexSubscriptionFailures.Lock()
	defer codexSubscriptionFailures.Unlock()
	codexSubscriptionFailures.at[subjectID] = at
}

func forgetCodexSubscriptionFailure(subjectID string) {
	codexSubscriptionFailures.Lock()
	defer codexSubscriptionFailures.Unlock()
	delete(codexSubscriptionFailures.at, subjectID)
}

// parseCodexEntitlement reads one workspace's billing cycle from an
// accounts/check response. The response keys workspaces by id and repeats the
// default one under "default"; only an entry naming the same account id is
// trusted, so another workspace's cycle is never stored on this account.
func parseCodexEntitlement(body []byte, accountID string, checked time.Time) (usage.AIAccountSubscription, bool) {
	var entry gjson.Result
	gjson.GetBytes(body, "accounts").ForEach(func(_, candidate gjson.Result) bool {
		if strings.TrimSpace(candidate.Get("account.account_id").String()) != accountID {
			return true
		}
		entry = candidate
		return false
	})
	entitlement := entry.Get("entitlement")
	if !entitlement.IsObject() {
		return usage.AIAccountSubscription{}, false
	}
	checked = checked.UTC()
	subscription := usage.AIAccountSubscription{
		Source:    usage.SubscriptionSourceProbe,
		ExpiresAt: parseProviderClaimTime(entitlement.Get("expires_at").Value()),
		RenewsAt:  parseProviderClaimTime(entitlement.Get("renews_at").Value()),
		CancelsAt: parseProviderClaimTime(entitlement.Get("cancels_at").Value()),
		Period:    strings.ToLower(strings.TrimSpace(entitlement.Get("billing_period").String())),
		CheckedAt: &checked,
	}
	active := entitlement.Get("has_active_subscription")
	willRenew := entry.Get("last_active_subscription.will_renew")
	switch {
	case active.Exists() && !active.Bool():
		subscription.State = usage.SubscriptionStateInactive
	case entitlement.Get("is_delinquent").Bool():
		subscription.State = usage.SubscriptionStatePastDue
	case subscription.CancelsAt != nil || willRenew.Type == gjson.False:
		subscription.State = usage.SubscriptionStateCanceling
	default:
		subscription.State = usage.SubscriptionStateActive
	}
	// expires_at is when paid access ends; it trails renews_at by a few hours
	// (live 2026-11-20: 11:03 against 05:03 UTC). Fall back to the cancel or
	// renewal date when the provider leaves it out.
	if subscription.ExpiresAt == nil {
		subscription.ExpiresAt = subscription.CancelsAt
	}
	if subscription.ExpiresAt == nil {
		subscription.ExpiresAt = subscription.RenewsAt
	}
	return subscription, true
}

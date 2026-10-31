package auth

import (
	"context"
	"sort"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ResetAwareSelector spends first the quota a reset is about to wipe out.
//
// A subscription window's unused share is gone once the window renews, so of
// the accounts with room left, the one whose window renews soonest takes the
// traffic: what it has left is about to expire, while an account renewing later
// keeps whatever it is spared. The longest window decides (the week, not the
// five hours inside it), and the next longest only breaks a tie. Renewals are
// compared to the hour, so a few minutes' difference does not reorder the pool
// from one request to the next.
//
// An account past resetAwareLowShare of a window the request draws from goes
// after every account with room, the least used first, and one past
// resetAwareSpentShare goes last: running an account into its cap only turns the
// next requests into 429s while other accounts could still answer. An account
// without quota data goes after the accounts known to have room and before the
// ones known to be nearly spent.
//
// Accounts that tie keep a fixed order (higher weight first, then ID), so the
// same account keeps answering and keeps the upstream's prompt cache warm. Like
// fill-first this concentrates traffic on one account at a time; an account at
// its concurrency limit is skipped by the execution path, and the next one in
// the order takes the request.
type ResetAwareSelector struct {
	deps *schedulerDeps
}

const (
	resetAwareLowShare   = 0.90
	resetAwareSpentShare = 0.98
)

// resetAwareTier groups accounts by how much room the request's windows leave,
// in the order the tiers are served.
type resetAwareTier int

const (
	resetAwareRoom resetAwareTier = iota
	resetAwareUnknown
	resetAwareLow
	resetAwareSpent
)

// resetAwareStanding is what the order is decided on for one account.
type resetAwareStanding struct {
	auth   *Auth
	weight int
	tier   resetAwareTier
	// used is the share spent of the most used window the request draws from.
	used float64
	// resets lists the windows' renewals to the hour, longest window first;
	// a zero entry is a window whose renewal is unknown.
	resets []time.Time
}

func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	ranked := rankResetAware(available, model, s.deps.quotaWindows())
	if len(ranked) == 0 {
		return nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	best := ranked[0].auth
	s.deps.observeSelection(best, now)
	return best, nil
}

// rankResetAware orders candidates the way ResetAwareSelector serves them.
func rankResetAware(candidates []*Auth, model string, source QuotaWindowSource) []resetAwareStanding {
	modelKey := canonicalModelKey(model)
	standings := make([]resetAwareStanding, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		var windows []QuotaWindow
		known := false
		if source != nil {
			windows, known = source.QuotaWindows(candidate, modelKey)
		}
		standings = append(standings, resetAwareStandingOf(candidate, windows, known))
	}
	sort.SliceStable(standings, func(i, j int) bool {
		return resetAwareBefore(standings[i], standings[j])
	})
	return standings
}

func resetAwareStandingOf(auth *Auth, windows []QuotaWindow, known bool) resetAwareStanding {
	standing := resetAwareStanding{auth: auth, weight: authSelectionWeight(auth), tier: resetAwareUnknown}
	if !known || len(windows) == 0 {
		return standing
	}
	ordered := make([]QuotaWindow, len(windows))
	copy(ordered, windows)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Span != b.Span {
			// A window of unknown length (0) cannot claim to be the longest.
			return a.Span > b.Span
		}
		return a.Key < b.Key
	})
	standing.resets = make([]time.Time, len(ordered))
	for i, window := range ordered {
		if window.Used > standing.used {
			standing.used = window.Used
		}
		if !window.ResetAt.IsZero() {
			standing.resets[i] = window.ResetAt.Truncate(time.Hour)
		}
	}
	switch {
	case standing.used >= resetAwareSpentShare:
		standing.tier = resetAwareSpent
	case standing.used >= resetAwareLowShare:
		standing.tier = resetAwareLow
	default:
		standing.tier = resetAwareRoom
	}
	return standing
}

// resetAwareBefore reports whether a is served before b.
func resetAwareBefore(a, b resetAwareStanding) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	switch a.tier {
	case resetAwareRoom:
		if before, decided := renewsBefore(a.resets, b.resets); decided {
			return before
		}
	case resetAwareLow, resetAwareSpent:
		if a.used != b.used {
			return a.used < b.used
		}
	}
	if a.weight != b.weight {
		return a.weight > b.weight
	}
	return a.auth.ID < b.auth.ID
}

// renewsBefore compares two renewal lists window by window, longest first. A
// known renewal goes before an unknown one; decided is false when every
// position renews in the same hour.
func renewsBefore(a, b []time.Time) (before, decided bool) {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y time.Time
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		switch {
		case x.Equal(y):
			continue
		case x.IsZero():
			return false, true
		case y.IsZero():
			return true, true
		}
		return x.Before(y), true
	}
	return false, false
}

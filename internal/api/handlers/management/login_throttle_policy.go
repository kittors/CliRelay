package management

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// throttleWindow is one observation window of a sliding-window counter.
// A zero Limit disables the window entirely.
type throttleWindow struct {
	Limit  int
	Window time.Duration
}

// throttlePolicy is the full OWASP "threshold + observation window + duration"
// triple for one bucket, plus the escalation ladder.
//
// Two windows are used instead of one because a single 15m/5 window still
// permits ~384 guesses per day against one account, which is enough for a
// password-spraying run. The long window caps the daily budget while the short
// window keeps the response to a burst immediate.
type throttlePolicy struct {
	Short throttleWindow
	// Long is the daily ceiling. Limit == 0 disables it.
	Long throttleWindow
	// Backoff is the staged block duration ladder, indexed by generation.
	Backoff []time.Duration
	// ResetAfter is the quiet period that clears counters and escalation.
	ResetAfter time.Duration
	// HardBlock allows a 403 ban instead of a 429. Only meaningful for buckets
	// that can only be filled by comparing a wrong secret from a single client.
	HardBlock bool
}

const (
	defaultLoginFailureWindow        = 15 * time.Minute
	defaultAccountFailureLimit       = 5
	defaultManagementKeyFailureLimit = 10
	defaultUnauthenticatedLimit      = 300
	defaultFailureResetAfter         = 12 * time.Hour
)

// defaultThrottlePolicies is the shipped policy table.
//
// Rationale for the specific numbers: ASVS V2.2.1's "100 per hour per account"
// is a ceiling rather than a target, so the admin surface sits far below it;
// Keycloak's default Failure Reset Time is 12h; Auth0's suspicious-IP default is
// 100/day. scopeUnauthenticated is deliberately the only high-volume bucket and
// deliberately never hard-blocks: a request that carried no secret proves
// nothing about the secret, so it may throttle but must never ban.
func defaultThrottlePolicies() map[throttleScope]throttlePolicy {
	return map[throttleScope]throttlePolicy{
		scopeUnauthenticated: {
			Short:      throttleWindow{Limit: defaultUnauthenticatedLimit, Window: time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute},
			ResetAfter: 2 * time.Hour,
			HardBlock:  false,
		},
		scopeManagementKey: {
			Short:      throttleWindow{Limit: defaultManagementKeyFailureLimit, Window: defaultLoginFailureWindow},
			Long:       throttleWindow{Limit: 100, Window: 24 * time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute},
			ResetAfter: defaultFailureResetAfter,
			HardBlock:  true,
		},
		scopeUserPassword: {
			Short:      throttleWindow{Limit: 20, Window: defaultLoginFailureWindow},
			Long:       throttleWindow{Limit: 100, Window: 24 * time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
			ResetAfter: defaultFailureResetAfter,
			HardBlock:  false,
		},
		scopeUserAccount: {
			Short:      throttleWindow{Limit: defaultAccountFailureLimit, Window: defaultLoginFailureWindow},
			Long:       throttleWindow{Limit: 50, Window: 24 * time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute},
			ResetAfter: defaultFailureResetAfter,
			HardBlock:  false,
		},
		scopeRefresh: {
			Short:      throttleWindow{Limit: 60, Window: 5 * time.Minute},
			Backoff:    []time.Duration{time.Minute},
			ResetAfter: time.Hour,
			HardBlock:  false,
		},
		scopePortalPassword: {
			Short:      throttleWindow{Limit: 20, Window: defaultLoginFailureWindow},
			Long:       throttleWindow{Limit: 100, Window: 24 * time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
			ResetAfter: defaultFailureResetAfter,
			HardBlock:  false,
		},
		scopePortalAccount: {
			Short:      throttleWindow{Limit: defaultAccountFailureLimit, Window: defaultLoginFailureWindow},
			Long:       throttleWindow{Limit: 50, Window: 24 * time.Hour},
			Backoff:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute},
			ResetAfter: defaultFailureResetAfter,
			HardBlock:  false,
		},
	}
}

// throttlePoliciesFromConfig overlays remote-management.auth onto the defaults.
// Every knob is zero-means-default so an absent config block behaves exactly
// like the shipped table, and non-positive overrides are ignored rather than
// disabling a bucket (a limit of 0 would otherwise ban on the first request).
func throttlePoliciesFromConfig(cfg *config.Config) map[throttleScope]throttlePolicy {
	policies := defaultThrottlePolicies()
	if cfg == nil {
		return policies
	}
	auth := cfg.RemoteManagement.Auth

	if seconds := auth.LoginFailureWindowSeconds; seconds > 0 {
		window := time.Duration(seconds) * time.Second
		for _, scope := range credentialGuessScopes {
			policy := policies[scope]
			policy.Short.Window = window
			policies[scope] = policy
		}
	}
	if hours := auth.FailureResetHours; hours > 0 {
		reset := time.Duration(hours) * time.Hour
		for _, scope := range credentialGuessScopes {
			policy := policies[scope]
			policy.ResetAfter = reset
			policies[scope] = policy
		}
	}
	if limit := auth.AccountFailureLimit; limit > 0 {
		for _, scope := range []throttleScope{scopeUserAccount, scopePortalAccount} {
			policy := policies[scope]
			policy.Short.Limit = limit
			policies[scope] = policy
		}
	}
	if limit := auth.ManagementKeyFailureLimit; limit > 0 {
		policy := policies[scopeManagementKey]
		policy.Short.Limit = limit
		policies[scopeManagementKey] = policy
	}
	if limit := auth.UnauthenticatedRequestLimit; limit > 0 {
		policy := policies[scopeUnauthenticated]
		policy.Short.Limit = limit
		policies[scopeUnauthenticated] = policy
	}
	return policies
}

// credentialGuessScopes are the buckets that only fill when a stored secret was
// actually compared. scopeUnauthenticated and scopeRefresh are excluded: they
// carry no guess signal, so the operator-facing "login failure window" knob must
// not stretch their much shorter windows.
var credentialGuessScopes = []throttleScope{
	scopeManagementKey,
	scopeUserPassword,
	scopeUserAccount,
	scopePortalPassword,
	scopePortalAccount,
}

// blockDurationFor picks the staged block duration for the given generation,
// clamping to the last rung so escalation plateaus instead of overflowing.
func blockDurationFor(p throttlePolicy, generation int) time.Duration {
	if len(p.Backoff) == 0 {
		return 0
	}
	if generation < 0 {
		generation = 0
	}
	if generation >= len(p.Backoff) {
		generation = len(p.Backoff) - 1
	}
	return p.Backoff[generation]
}

// applySharedKeyDowngrade neutralises the "one wrong password bans everyone"
// failure mode: when the client IP cannot identify a single client, a hard ban
// would take down every user behind that proxy/NAT. The account-scoped bucket
// stays at full strength, which is the layer that actually stops guessing.
func applySharedKeyDowngrade(p throttlePolicy) throttlePolicy {
	p.HardBlock = false
	if len(p.Backoff) > 1 {
		p.Backoff = p.Backoff[:1]
	}
	if p.Long.Limit > 0 {
		p.Long.Limit *= 2
	}
	return p
}

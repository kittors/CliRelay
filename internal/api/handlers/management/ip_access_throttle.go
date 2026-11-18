package management

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ipaccess"
)

// applyThrottleOverride rebuilds the limiter's policy table from config and then
// layers the operator's runtime overrides on top.
//
// It rebuilds from config rather than mutating the live table so that clearing
// an override in the panel actually restores the configured value, instead of
// leaving the last non-zero number in place forever.
func (h *Handler) applyThrottleOverride(override ipaccess.ThrottleOverride) {
	if h == nil {
		return
	}
	policies := throttlePoliciesFromConfig(h.cfg)
	policies = overlayThrottleOverride(policies, override)
	h.loginThrottle.setPolicies(policies)
}

// storedThrottleOverride reads the persisted override, so a config reload does
// not silently discard settings an operator made in the panel.
func storedThrottleOverride() ipaccess.ThrottleOverride {
	registry := ipaccess.Default()
	if registry == nil {
		return ipaccess.ThrottleOverride{}
	}
	return registry.Policy().Throttle
}

// throttlePolicySnapshot renders the effective limits for display. The panel
// shows what is actually in force, not what was configured, because the two
// differ whenever an override is set or a bucket is shared-key downgraded.
func (h *Handler) throttlePolicySnapshot() []gin.H {
	if h == nil || h.loginThrottle == nil {
		return nil
	}
	snapshot := h.loginThrottle.policySnapshot()
	scopes := []throttleScope{
		scopeUnauthenticated, scopeManagementKey, scopeUserPassword,
		scopeUserAccount, scopeRefresh, scopePortalPassword, scopePortalAccount,
	}
	rendered := make([]gin.H, 0, len(scopes))
	for _, scope := range scopes {
		policy, ok := snapshot[scope]
		if !ok {
			policy = defaultThrottlePolicies()[scope]
		}
		backoff := make([]string, 0, len(policy.Backoff))
		for _, step := range policy.Backoff {
			backoff = append(backoff, step.String())
		}
		rendered = append(rendered, gin.H{
			"scope":         scope.String(),
			"short_limit":   policy.Short.Limit,
			"short_window":  policy.Short.Window.String(),
			"long_limit":    policy.Long.Limit,
			"long_window":   policy.Long.Window.String(),
			"backoff":       backoff,
			"reset_after":   policy.ResetAfter.String(),
			"hard_block":    policy.HardBlock,
			"key_dimension": throttleKeyDimension(scope),
		})
	}
	return rendered
}

// throttleKeyDimension names what a bucket counts against. The panel shows it
// because "too many login attempts" is ambiguous to the person reading it: the
// per-IP and per-account buckets return the identical error code, and knowing
// which one is armed is the difference between "wait" and "someone is guessing
// my password".
func throttleKeyDimension(scope throttleScope) string {
	switch scope {
	case scopeUserAccount, scopePortalAccount:
		return "username"
	default:
		return "client_ip"
	}
}

// overlayThrottleOverride applies operator overrides to a policy table. A zero
// value means "keep whatever config or the shipped defaults said", so an
// untouched field never rewrites configured behaviour.
func overlayThrottleOverride(policies map[throttleScope]throttlePolicy, override ipaccess.ThrottleOverride) map[throttleScope]throttlePolicy {
	if seconds := override.LoginFailureWindowSeconds; seconds > 0 {
		window := time.Duration(seconds) * time.Second
		for _, scope := range credentialGuessScopes {
			policy := policies[scope]
			policy.Short.Window = window
			policies[scope] = policy
		}
	}
	if hours := override.FailureResetHours; hours > 0 {
		reset := time.Duration(hours) * time.Hour
		for _, scope := range credentialGuessScopes {
			policy := policies[scope]
			policy.ResetAfter = reset
			policies[scope] = policy
		}
	}
	if limit := override.AccountFailureLimit; limit > 0 {
		for _, scope := range []throttleScope{scopeUserAccount, scopePortalAccount} {
			policy := policies[scope]
			policy.Short.Limit = limit
			policies[scope] = policy
		}
	}
	if limit := override.ManagementKeyFailureLimit; limit > 0 {
		policy := policies[scopeManagementKey]
		policy.Short.Limit = limit
		policies[scopeManagementKey] = policy
	}
	if limit := override.UnauthenticatedRequestLimit; limit > 0 {
		policy := policies[scopeUnauthenticated]
		policy.Short.Limit = limit
		policies[scopeUnauthenticated] = policy
	}
	return policies
}

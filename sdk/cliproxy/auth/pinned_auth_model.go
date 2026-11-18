package auth

import (
	"strings"
	"time"
)

// AuthServesModel reports whether the credential authID could be selected to
// serve model for one of providers on behalf of tenantID.
//
// It applies only the checks that do not change from one request to the next:
// provider, enablement, tenant visibility and the model list, using the same
// model-support rule as selection (thinking suffixes stripped, channel-group
// prefixes and credentials without a registered model list handled). Cooldown,
// quota and concurrency are left to PinnedAuthHeldBack, which keeps a pin
// through a moment's back-off or a busy account and lets it go when the
// account is out of service for longer.
func (m *Manager) AuthServesModel(tenantID, authID string, providers []string, model string) bool {
	authID = strings.TrimSpace(authID)
	modelKey := normalizeSelectionModelKey(model)
	if m == nil || authID == "" || modelKey == "" {
		return false
	}
	tenantID = normalizedTenantID(tenantID)
	cfg := m.currentRuntimeConfigForTenant(tenantID)
	grants := m.sharedGrantIndex()

	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil || auth.Disabled || auth.Status == StatusDisabled {
		return false
	}
	if !authUsableByTenant(auth, tenantID, grants) {
		return false
	}
	if len(providers) > 0 {
		authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
		matched := false
		for _, provider := range providers {
			if strings.ToLower(strings.TrimSpace(provider)) == authProvider {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return candidateSupportsModel(cfg, m.modelRegistry, auth, modelKey, "", nil)
}

// PinnedAuthHeldBack reports whether authID is held back from model now and
// will not be let through again within the optimistic retry window. A request
// pinned to it then fails without reaching the upstream (model_cooldown or
// model_unavailable), although another credential of the pool may serve it.
// reason names the cause for the log, e.g. "quota_exhausted until <time>".
//
// It is the half of the pin check AuthServesModel leaves out, and it spares the
// moments a pin is worth keeping through: a back-off short enough that the
// selector still lets the credential through, and a saturated account, whose
// request queues. Moving then would throw away the upstream state the pin keeps.
// An exhausted quota, a rejected credential or a disabled one does not pass that
// soon; keeping the pin would fail every later request of the session until the
// client reconnects, while the rest of the pool could serve them.
func (m *Manager) PinnedAuthHeldBack(authID, model string) (bool, string) {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" {
		return false, ""
	}
	now := time.Now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		// A credential that is gone fails AuthServesModel.
		return false, ""
	}
	block := authBlockForModel(auth, strings.TrimSpace(model), now)
	if !block.blocked {
		return false, ""
	}
	// The selector lets a credential through a non-quota block that ends this
	// soon (earliestOptimisticCandidate), so the pinned request still runs.
	if block.reason == blockReasonOther && !block.next.IsZero() && !block.next.After(now.Add(optimisticRetryWindow)) {
		return false, ""
	}
	return true, describePinBlock(block)
}

func describePinBlock(block authBlock) string {
	if block.reason == blockReasonDisabled {
		return "disabled"
	}
	var quota QuotaState
	if block.quota != nil {
		quota = *block.quota
	}
	reason := CooldownReasonOf(block.lastErr, quota)
	if reason == "" {
		reason = "cooling down"
	}
	if block.next.IsZero() {
		return reason
	}
	return reason + " until " + block.next.Format(time.RFC3339)
}

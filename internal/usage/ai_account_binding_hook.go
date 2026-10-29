package usage

import (
	"context"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// AIAccountBindingHook owns only the low-frequency A-layer binding path, plus the
// live quota readings the scheduler hands over (OnQuotaObserved). The
// per-request usage projection receives auth_subject_id directly from the
// selected auth and must never call this hook or query the binding table; the
// scheduler calls OnQuotaObserved off the request path and coalesced to one call
// per account per interval, so it is not part of that projection either.
type AIAccountBindingHook struct {
	coreauth.NoopHook
}

func NewAIAccountBindingHook() coreauth.Hook { return AIAccountBindingHook{} }

// The scheduler finds the optional extension by type assertion, which fails
// silently if a signature drifts; this makes drift a compile error.
var _ coreauth.QuotaObservationHook = AIAccountBindingHook{}

func (AIAccountBindingHook) OnAuthRegistered(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}
func (AIAccountBindingHook) OnAuthUpdated(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}
func (AIAccountBindingHook) OnAuthLoaded(_ context.Context, auth *coreauth.Auth) {
	reconcileAIAccountBinding(auth)
}

// OnQuotaObserved implements coreauth.QuotaObservationHook: it puts the quota
// windows an executor read off a live upstream answer onto the console's quota
// card (see MergeObservedAIAccountQuota).
func (AIAccountBindingHook) OnQuotaObserved(_ context.Context, auth *coreauth.Auth, observation cliproxyexecutor.QuotaObservation) {
	if err := MergeObservedAIAccountQuota(auth, observation); err != nil && auth != nil {
		log.WithError(err).WithFields(log.Fields{
			"tenant_id": normalizeTenantID(auth.TenantID), "provider": auth.Provider,
		}).Warn("usage: merge live ai account quota")
	}
}

func (AIAccountBindingHook) OnAuthDeleted(_ context.Context, auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if err := MarkAIAccountTenantBindingDeleted(auth.TenantID, auth.ID); err != nil {
		log.WithError(err).WithField("tenant_id", normalizeTenantID(auth.TenantID)).Warn("usage: soft-delete ai account binding")
	}
}

func reconcileAIAccountBinding(auth *coreauth.Auth) {
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		return
	}
	// Runs before the binding upsert so the account's history is already under the
	// shared id by the time anything points at it.
	if err := MergeLegacySplitAuthSubject(auth, identity); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"tenant_id": normalizeTenantID(auth.TenantID), "provider": identity.Provider,
			"auth_subject_id": identity.ID,
		}).Warn("usage: merge legacy split ai account subject")
	}
	if err := UpsertAIAccountTenantBinding(auth, identity); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"tenant_id": normalizeTenantID(auth.TenantID), "provider": identity.Provider,
			"auth_subject_id": identity.ID,
		}).Warn("usage: reconcile ai account binding")
	}
}

package auth

import (
	"context"
	"strings"
	"time"
)

// ClearModelCooldown lifts the runtime cooldown of one model on one account, so
// the next request may try it at once. ClearQuotaStatus does the same for the
// whole account; this is for the operator who knows one model recovered (a
// provider-side incident ended, a plan was upgraded) and does not want to
// release the account's other models with it.
//
// Nothing else is touched: the account's other model states, its credential and
// the refresh schedule stay as they were. If the upstream still refuses, the
// next failure sets a new cooldown.
//
// It reports whether anything changed. An unknown account, a model without a
// state, and a model that is not held back all report false with no error.
func (m *Manager) ClearModelCooldown(ctx context.Context, id, model string) (bool, error) {
	if m == nil {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	model = strings.TrimSpace(model)
	if id == "" || model == "" {
		return false, nil
	}

	now := time.Now()
	var (
		updated *Auth
		cleared string
	)

	m.mu.Lock()
	current, ok := m.auths[id]
	if !ok || current == nil {
		m.mu.Unlock()
		return false, nil
	}
	key, state := lookupModelState(current, model)
	if state != nil {
		previous := state.LastError
		if clearManualModelRuntimeState(state, now) {
			if state.Status != StatusDisabled {
				cleared = key
			}
			updateAggregatedAvailability(current, now)
			releaseAuthErrorAfterModelClear(current, previous, now)
			current.UpdatedAt = now
			syncPersistedQuotaRuntime(current)
			updated = current.Clone()
		}
	}
	m.mu.Unlock()

	if updated == nil {
		return false, nil
	}
	if errPersist := m.persist(ctx, updated); errPersist != nil {
		return true, errPersist
	}
	m.hook.OnAuthUpdated(ctx, updated.Clone())
	if cleared != "" {
		m.resumeRecoveredQuotaModels(id, []string{cleared})
	}
	return true, nil
}

// lookupModelState finds the state recorded for model. States are keyed by the
// model name a request carried, which may include a thinking suffix, while the
// panel and the selector both fall back to the base name.
func lookupModelState(auth *Auth, model string) (string, *ModelState) {
	if state := auth.ModelStates[model]; state != nil {
		return model, state
	}
	base := canonicalModelKey(model)
	for key, state := range auth.ModelStates {
		if state != nil && canonicalModelKey(key) == base {
			return key, state
		}
	}
	return "", nil
}

// releaseAuthErrorAfterModelClear drops the account-level error a model's failure
// left behind once nothing else explains it. The account carries the last error
// of whichever model failed most recently; keeping it after that model was
// cleared would show an error badge on an account with nothing wrong.
func releaseAuthErrorAfterModelClear(auth *Auth, cleared *Error, now time.Time) {
	if auth == nil || auth.Disabled || auth.Status == StatusDisabled || cleared == nil {
		return
	}
	if !errorsEqual(auth.LastError, cleared) || hasModelError(auth, now) {
		return
	}
	auth.LastError = nil
	auth.StatusMessage = ""
	if auth.Status == StatusError {
		auth.Status = StatusActive
	}
}

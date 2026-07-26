package auth

import (
	"encoding/json"
	"time"
)

const persistedQuotaRuntimeMetadataKey = "_cli_proxy_runtime_quota"

type persistedQuotaRuntime struct {
	Status         Status                                `json:"status,omitempty"`
	StatusMessage  string                                `json:"status_message,omitempty"`
	NextRetryAfter time.Time                             `json:"next_retry_after,omitempty"`
	Quota          QuotaState                            `json:"quota"`
	ModelStates    map[string]persistedQuotaModelRuntime `json:"model_states,omitempty"`
}

type persistedQuotaModelRuntime struct {
	Status         Status     `json:"status,omitempty"`
	StatusMessage  string     `json:"status_message,omitempty"`
	NextRetryAfter time.Time  `json:"next_retry_after,omitempty"`
	Quota          QuotaState `json:"quota"`
}

func syncPersistedQuotaRuntime(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}

	runtime := persistedQuotaRuntime{
		Status:         auth.Status,
		StatusMessage:  auth.StatusMessage,
		NextRetryAfter: auth.NextRetryAfter,
		Quota:          auth.Quota,
	}
	for model, state := range auth.ModelStates {
		if state == nil || !quotaRecoveryGateArmed(state.Quota) {
			continue
		}
		if runtime.ModelStates == nil {
			runtime.ModelStates = make(map[string]persistedQuotaModelRuntime)
		}
		runtime.ModelStates[model] = persistedQuotaModelRuntime{
			Status:         state.Status,
			StatusMessage:  state.StatusMessage,
			NextRetryAfter: state.NextRetryAfter,
			Quota:          state.Quota,
		}
	}

	if !quotaRecoveryGateArmed(runtime.Quota) && len(runtime.ModelStates) == 0 {
		delete(auth.Metadata, persistedQuotaRuntimeMetadataKey)
		return
	}
	auth.Metadata[persistedQuotaRuntimeMetadataKey] = runtime
}

func restorePersistedQuotaRuntime(auth *Auth) {
	if auth == nil || auth.Metadata == nil || auth.Disabled || auth.Status == StatusDisabled {
		return
	}
	raw, ok := auth.Metadata[persistedQuotaRuntimeMetadataKey]
	if !ok || raw == nil {
		return
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var runtime persistedQuotaRuntime
	if err = json.Unmarshal(data, &runtime); err != nil {
		return
	}
	if !quotaRecoveryGateArmed(runtime.Quota) && len(runtime.ModelStates) == 0 {
		return
	}

	if quotaRecoveryGateArmed(runtime.Quota) {
		auth.Quota = runtime.Quota
		auth.NextRetryAfter = runtime.NextRetryAfter
		auth.Unavailable = true
		auth.Status = runtime.Status
		if auth.Status == "" || auth.Status == StatusActive || auth.Status == StatusUnknown {
			auth.Status = StatusError
		}
		auth.StatusMessage = runtime.StatusMessage
		if auth.StatusMessage == "" {
			auth.StatusMessage = "quota exhausted"
		}
	}
	if len(runtime.ModelStates) == 0 {
		return
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState, len(runtime.ModelStates))
	}
	for model, persisted := range runtime.ModelStates {
		if !quotaRecoveryGateArmed(persisted.Quota) {
			continue
		}
		state := &ModelState{
			Status:         persisted.Status,
			StatusMessage:  persisted.StatusMessage,
			Unavailable:    true,
			NextRetryAfter: persisted.NextRetryAfter,
			Quota:          persisted.Quota,
		}
		if state.Status == "" || state.Status == StatusActive || state.Status == StatusUnknown {
			state.Status = StatusError
		}
		if state.StatusMessage == "" {
			state.StatusMessage = "quota exhausted"
		}
		auth.ModelStates[model] = state
	}
	updateAggregatedAvailability(auth, time.Now())
}

// quotaRecoveryGateArmed reports whether a probe-confirmed quota gate exists at
// all, ignoring its deadline. Persistence must not drop a gate just because its
// deadline is close: expiry is re-evaluated against the clock on every check.
func quotaRecoveryGateArmed(quota QuotaState) bool {
	return quota.Exceeded && quota.RecoveryRequired
}

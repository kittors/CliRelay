package management

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	apikeysettings "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/apikey"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// alertSources reads what the alert rules judge from data that is already
// stored: the credentials the auth manager holds in memory, the account status
// the probes keep current, and the usage tables the quota admission reads. It
// triggers no probe and writes nothing, so evaluating alerts never costs an
// upstream call. (aiaccountstatus.Service.ListStatus is not used for that
// reason: it also reconciles bindings and normalizes stale state.)
type alertSources struct {
	authManager func() *coreauth.Manager
}

var _ alerting.Sources = alertSources{}

// transientFailureMemory is how recent the last failure of an account has to be
// for an upstream timeout or 5xx to count as "still failing". A credential keeps
// the error of its last failed request until a later request succeeds, and one
// that is never picked again (fill-first scheduling prefers the others) would
// otherwise look broken for good after a single blip. Refusals that need a
// person (401, 402, 403, 404) are not aged out: they are exactly what an idle
// account hides.
const transientFailureMemory = 30 * time.Minute

// Accounts returns the tenant's upstream accounts that are switched on, with
// their quota windows and, for those that are not working, why.
func (s alertSources) Accounts(_ context.Context, tenantID string, now time.Time) ([]alerting.AccountSample, error) {
	var manager *coreauth.Manager
	if s.authManager != nil {
		manager = s.authManager()
	}
	if manager == nil {
		return nil, nil
	}
	type entry struct {
		auth    *coreauth.Auth
		subject string
	}
	auths := manager.ListForTenant(tenantID)
	entries := make([]entry, 0, len(auths))
	subjectIDs := make([]string, 0, len(auths))
	seen := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		// An account the operator switched off is not supposed to work.
		if auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled {
			continue
		}
		subject := ""
		if identity := usage.ResolveAuthSubjectIdentity(auth); identity != nil {
			subject = identity.ID
		}
		entries = append(entries, entry{auth: auth, subject: subject})
		if _, dup := seen[subject]; subject != "" && !dup {
			seen[subject] = struct{}{}
			subjectIDs = append(subjectIDs, subject)
		}
	}
	statusBySubject := make(map[string]usage.AIAccountSubjectStatusRecord, len(subjectIDs))
	if len(subjectIDs) > 0 {
		rows, err := usage.ListAIAccountSubjectStatus(subjectIDs)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			statusBySubject[row.AuthSubjectID] = row
		}
	}
	out := make([]alerting.AccountSample, 0, len(entries))
	for _, e := range entries {
		sample := alerting.AccountSample{
			ID:        e.auth.ID,
			Subject:   e.subject,
			Provider:  strings.ToLower(strings.TrimSpace(e.auth.Provider)),
			Name:      accountDisplayName(e.auth),
			StaticKey: isStaticKeyCredential(e.auth),
			Failure:   accountFailure(e.auth, now),
		}
		if row, ok := statusBySubject[e.subject]; ok {
			sample.Windows = quotaWindowSamples(row.Quotas, row.QuotaObservedAt)
		}
		out = append(out, sample)
	}
	return out, nil
}

// isStaticKeyCredential reports whether the credential is a plain API key rather
// than a signed-in account. Only the kind is read: the second value AccountInfo
// returns for a key is the key itself, and is discarded here.
func isStaticKeyCredential(auth *coreauth.Auth) bool {
	kind, _ := auth.AccountInfo()
	return kind == "api_key"
}

// accountDisplayName is what an operator knows the account as: the label they
// gave it (the channel name, for a configured key), else its login, else the
// file it was imported from. It never reads the key of an API-key credential.
func accountDisplayName(auth *coreauth.Auth) string {
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if value, ok := auth.Metadata["label"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	for _, key := range []string{"email", "account_email"} {
		if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	return auth.ID
}

// quotaWindowSamples turns the stored windows into samples. The status keeps
// the share remaining; the rules think in the share used.
func quotaWindowSamples(windows []usage.QuotaWindowDTO, rowObservedAt *time.Time) []alerting.WindowSample {
	out := make([]alerting.WindowSample, 0, len(windows))
	for _, dto := range windows {
		// A window with no percentage says nothing about how much room is left.
		if dto.Percent == nil {
			continue
		}
		remaining := min(max(*dto.Percent, 0), 100)
		sample := alerting.WindowSample{
			Key:         dto.QuotaKey,
			Label:       dto.QuotaLabel,
			Span:        time.Duration(dto.WindowSeconds) * time.Second,
			UsedPercent: 100 - remaining,
			// The scheduler's own rule for "limits every request of the account", so
			// the pool view adds up the same windows that decide where traffic goes.
			AccountWide: aiaccountstatus.WindowAppliesToModel(dto.QuotaKey, ""),
		}
		if dto.ResetAt != nil {
			sample.ResetAt = dto.ResetAt.UTC()
		}
		switch {
		case dto.ObservedAt != nil:
			sample.ObservedAt = dto.ObservedAt.UTC()
		case rowObservedAt != nil:
			sample.ObservedAt = rowObservedAt.UTC()
		}
		out = append(out, sample)
	}
	return out
}

// failureRank orders the reasons an account can be held back by how much they
// say about the account itself. Reasons not listed (an exhausted quota, rate
// limiting, a token being renewed, one model missing) are not failures of the
// account: the quota rule covers the first, the others heal on their own.
var failureRank = map[string]int{
	coreauth.CooldownReasonReauthRequired:  5,
	coreauth.CooldownReasonUnauthorized:    4,
	coreauth.CooldownReasonPaymentRequired: 3,
	coreauth.CooldownReasonNotFound:        2,
	coreauth.CooldownReasonUpstreamError:   1,
}

// accountFailure judges whether an account is not working and why, from the
// state the failures left on the credential. Every failure path writes that
// state (a local failure, a peer node's notice, a provider-specific path), and
// the next success clears it, so "has an error and no success since" is what
// "not working" means here.
func accountFailure(auth *coreauth.Auth, now time.Time) alerting.FailureSample {
	hasError := auth.Status == coreauth.StatusError && auth.LastError != nil
	if !auth.Unavailable && !hasError {
		return alerting.FailureSample{}
	}
	reason, status, at := "", 0, auth.UpdatedAt
	if auth.LastError != nil {
		reason, status = coreauth.CooldownReasonOf(auth.LastError, auth.Quota), auth.LastError.HTTPStatus
	}
	if failureRank[reason] == 0 {
		// An account whose models are all held back carries no error of its own;
		// the cause is in the models.
		reason, status = "", 0
		for _, cooldown := range coreauth.ActiveModelCooldowns(auth, now) {
			if failureRank[cooldown.Reason] > failureRank[reason] {
				reason, status = cooldown.Reason, cooldown.HTTPStatus
			}
		}
		for _, state := range auth.ModelStates {
			if state != nil && state.UpdatedAt.After(at) {
				at = state.UpdatedAt
			}
		}
	}
	sample := alerting.FailureSample{Reason: reason, HTTPStatus: status}
	switch reason {
	case coreauth.CooldownReasonReauthRequired:
		sample.Class = alerting.FailureRevoked
	case coreauth.CooldownReasonUnauthorized, coreauth.CooldownReasonPaymentRequired, coreauth.CooldownReasonNotFound:
		sample.Class = alerting.FailureNeedsAction
	case coreauth.CooldownReasonUpstreamError:
		if !at.IsZero() && now.Sub(at) <= transientFailureMemory {
			sample.Class = alerting.FailureTransient
		}
	}
	if sample.Class == alerting.FailureNone {
		return alerting.FailureSample{}
	}
	return sample
}

// Spenders returns the tenant's keys and end users that have a limit, with how
// much of each limit is used. A key or user without any limit, or switched off,
// is left out before any usage is read.
func (s alertSources) Spenders(_ context.Context, tenantID string, now time.Time) ([]alerting.SpendSample, error) {
	rows := usage.ListAPIKeysForTenant(tenantID)
	if len(rows) == 0 {
		return nil, nil
	}
	profiles := usage.ListAPIKeyPermissionProfilesForTenant(tenantID)

	type limitedKey struct {
		row    usage.APIKeyRow
		matrix quota.Matrix
	}
	var keys []limitedKey
	var keyIDs, endUserIDs []string
	seenUsers := make(map[string]struct{})
	for _, raw := range rows {
		if raw.Disabled {
			continue
		}
		// The limit that applies is the one after the key's permission profile.
		row := usage.EffectiveAPIKeyRowWithProfiles(raw, profiles)
		if matrix := apikeysettings.KeyQuotaMatrix(row); !matrix.IsZero() && strings.TrimSpace(row.ID) != "" {
			keys = append(keys, limitedKey{row: row, matrix: matrix})
			keyIDs = append(keyIDs, strings.TrimSpace(row.ID))
		}
		if id := strings.TrimSpace(row.EndUserID); id != "" {
			if _, dup := seenUsers[id]; !dup {
				seenUsers[id] = struct{}{}
				endUserIDs = append(endUserIDs, id)
			}
		}
	}

	users, err := s.limitedEndUsers(tenantID, endUserIDs, profiles)
	if err != nil {
		return nil, err
	}

	out := make([]alerting.SpendSample, 0, len(keys)+len(users))
	if len(keys) > 0 {
		snapshots, err := usage.QueryAPIKeyQuotaUsageSnapshotsAt(tenantID, keyIDs, now)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			cells := spendCells(k.matrix, snapshots[strings.TrimSpace(k.row.ID)], now)
			if len(cells) == 0 {
				continue
			}
			out = append(out, alerting.SpendSample{
				Kind: alerting.SubjectKey, ID: strings.TrimSpace(k.row.ID),
				Name:  keyDisplayName(k.row, users[strings.TrimSpace(k.row.EndUserID)].quota.DisplayName),
				Cells: cells,
			})
		}
	}
	if len(users) > 0 {
		ids := make([]string, 0, len(users))
		for id := range users {
			ids = append(ids, id)
		}
		snapshots, err := usage.QueryEndUserQuotaUsageSnapshotsAt(tenantID, ids, now)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			cells := spendCells(users[id].matrix, snapshots[id], now)
			if len(cells) == 0 {
				continue
			}
			out = append(out, alerting.SpendSample{
				Kind: alerting.SubjectEndUser, ID: id, Name: strings.TrimSpace(users[id].quota.DisplayName), Cells: cells,
			})
		}
	}
	return out, nil
}

type limitedUser struct {
	quota  usage.EndUserQuota
	matrix quota.Matrix
}

// limitedEndUsers loads the end users behind the tenant's keys and keeps those
// that are active and have a limit. The ids come from the tenant's own keys and
// the tenant of each row is checked again, so no other tenant's user can end up
// in this tenant's alerts.
func (s alertSources) limitedEndUsers(tenantID string, ids []string, profiles []usage.APIKeyPermissionProfileRow) (map[string]limitedUser, error) {
	out := make(map[string]limitedUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	quotas, err := usage.ListEndUserQuotasByIDs(ids)
	if err != nil {
		return nil, err
	}
	for id, raw := range quotas {
		if !strings.EqualFold(strings.TrimSpace(raw.TenantID), strings.TrimSpace(tenantID)) || !strings.EqualFold(strings.TrimSpace(raw.Status), "active") {
			continue
		}
		effective := usage.EffectiveEndUserQuotaWithProfiles(raw, profiles)
		if matrix := effective.QuotaMatrix(); !matrix.IsZero() {
			out[id] = limitedUser{quota: effective, matrix: matrix}
		}
	}
	return out, nil
}

// keyDisplayName names a key by what the operator called it, prefixed with its
// owner's name for a key that belongs to an end user. It never falls back to the
// key itself: that is the secret.
func keyDisplayName(row usage.APIKeyRow, ownerName string) string {
	name := strings.TrimSpace(row.Name)
	if name == "" {
		id := strings.TrimSpace(row.ID)
		name = "key " + id[:min(len(id), 8)]
	}
	if owner := strings.TrimSpace(ownerName); owner != "" {
		return owner + " / " + name
	}
	return name
}

// spendCells pairs each limit of a matrix with the usage in the same cell, and
// with when its window renews: the calendar windows end at their boundary, the
// 5-hour window five hours after its anchor, and the lifetime limit never.
func spendCells(limits quota.Matrix, snapshot quota.UsageSnapshot, now time.Time) []alerting.SpendCell {
	cells := make([]alerting.SpendCell, 0, 4)
	for _, cell := range limits.ConfiguredCells() {
		spend := alerting.SpendCell{
			Metric: string(cell.Metric),
			Window: string(cell.Window),
			Used:   snapshot.Total.Value(cell.Metric, cell.Window),
			Limit:  cell.Value,
		}
		switch cell.Window {
		case quota.PeriodFiveHour:
			if !snapshot.FiveHourWindowStart.IsZero() {
				if end := snapshot.FiveHourWindowStart.Add(quota.FiveHourWindowDuration); end.After(now) {
					spend.ResetAt = end
				}
			}
		case quota.PeriodLifetime:
		default:
			if end, ok := usage.PeriodResetAt(cell.Window, now); ok {
				spend.ResetAt = end
			}
		}
		cells = append(cells, spend)
	}
	return cells
}

package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Live quota windows.
//
// A status probe is the console's source for an account's quota card, and it
// runs every few minutes at best. The upstream also reports the same windows on
// every request that account serves, and the scheduler hands those over as they
// arrive (coreauth.QuotaObservationHook). Folding them in here is what lets the
// card show the number from the last request instead of the last probe.
//
// Only the card's windows move. A live reading is not a probe: it says nothing
// about the account's plan, health or subscription, so those fields, the probe
// timestamps, and everything derived from probes (the quota history points,
// the cycle anchors) are left alone.

// subjectQuotaMu serialises every read-modify-write of
// ai_account_subject_status.quota_json. Probes are already serialised per account
// by the status service, but a live merge can land at any moment, and without a
// shared lock a probe's write based on the row it read before the merge would
// erase the merge, or the other way round.
var subjectQuotaMu sync.Mutex

// CodexQuotaWindowKey names a plan window of a Codex limit as the console card
// knows it, together with the label key it renders. prefix is "code" for the
// account's main limit and "review" for code review's. Both the status probe and
// the live path build their windows through it, so a window reported by either
// lands on the same card entry.
func CodexQuotaWindowKey(prefix string, seconds int64) (key, label string) {
	switch seconds {
	case 18000:
		return prefix + "_5h", "m_quota." + prefix + "_5h"
	case 604800:
		return prefix + "_week", "m_quota." + prefix + "_weekly"
	default:
		return fmt.Sprintf("%s_subscription_%d", prefix, seconds), "m_quota." + prefix + "_subscription"
	}
}

// liveQuotaWindows turns an observation into card windows. It returns nothing for
// a provider whose windows it cannot name.
func liveQuotaWindows(provider string, observation cliproxyexecutor.QuotaObservation) []QuotaWindowDTO {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return nil
	}
	windows := make([]QuotaWindowDTO, 0, len(observation.Windows))
	for _, observed := range observation.Windows {
		if observed.WindowMinutes <= 0 {
			// Without the length there is no telling which card entry it is.
			continue
		}
		seconds := int64(observed.WindowMinutes) * 60
		key, label := CodexQuotaWindowKey("code", seconds)
		remaining := 100 - math.Min(math.Max(observed.UsedPercent, 0), 100)
		window := QuotaWindowDTO{QuotaKey: key, QuotaLabel: label, WindowSeconds: seconds, Percent: &remaining}
		if !observed.ResetAt.IsZero() {
			resetAt := observed.ResetAt.UTC()
			window.ResetAt = &resetAt
		}
		windows = append(windows, window)
	}
	return windows
}

// MergeObservedAIAccountQuota folds the windows an executor read off a live
// answer into the account's shared status row. It does nothing for an account
// that has no row yet (a probe creates it, with the plan and health a live
// reading cannot supply) and for providers whose windows it cannot name.
func MergeObservedAIAccountQuota(auth *coreauth.Auth, observation cliproxyexecutor.QuotaObservation) error {
	identity := ResolveAuthSubjectIdentity(auth)
	if identity == nil {
		return nil
	}
	windows := liveQuotaWindows(identity.Provider, observation)
	db := getDB()
	if db == nil || len(windows) == 0 {
		return nil
	}
	observedAt := observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	_, err := mergeLiveQuotaWindows(db, identity.ID, windows, observedAt)
	return err
}

// mergeLiveQuotaWindows writes windows into the row for subjectID and reports
// whether the row changed.
func mergeLiveQuotaWindows(db *sql.DB, subjectID string, windows []QuotaWindowDTO, observedAt time.Time) (bool, error) {
	observedAt = observedAt.UTC()
	subjectQuotaMu.Lock()
	defer subjectQuotaMu.Unlock()

	previous, rowObserved, exists, err := loadSubjectQuotaRow(db, subjectID)
	if err != nil || !exists {
		return false, err
	}
	incoming := windowsNotSupersededBy(previous, windows, observedAt)
	if len(incoming) == 0 {
		return false, nil
	}
	merged := mergeQuotaWindows(previous, incoming, observedAt)
	rowStamp := observedAt
	if rowObserved != nil && rowObserved.After(rowStamp) {
		rowStamp = rowObserved.UTC()
	}
	quotaJSON, err := json.Marshal(merged)
	if err != nil {
		return false, err
	}
	stamp := rowStamp.Format(time.RFC3339Nano)
	if _, err := db.Exec(`
		UPDATE ai_account_subject_status
		SET quota_json = ?, quota_observed_at = ?, version = version + 1, updated_at = ?
		WHERE auth_subject_id = ?
	`, string(quotaJSON), stamp, time.Now().UTC().Format(time.RFC3339Nano), subjectID); err != nil {
		return false, fmt.Errorf("usage: merge live ai account quota: %w", err)
	}
	return true, nil
}

// windowsNotSupersededBy drops the windows whose stored copy was confirmed after
// observedAt. A reading waits for its turn in the scheduler's write queue, so a
// probe may have stored a newer answer for the same window in the meantime, and
// the older reading must not replace it.
func windowsNotSupersededBy(previous, incoming []QuotaWindowDTO, observedAt time.Time) []QuotaWindowDTO {
	stored := make(map[string]*time.Time, len(previous))
	for i := range previous {
		stored[strings.TrimSpace(previous[i].QuotaKey)] = previous[i].ObservedAt
	}
	kept := make([]QuotaWindowDTO, 0, len(incoming))
	for _, window := range incoming {
		if at := stored[strings.TrimSpace(window.QuotaKey)]; at != nil && at.After(observedAt) {
			continue
		}
		kept = append(kept, window)
	}
	return kept
}

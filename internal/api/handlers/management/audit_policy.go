package management

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Audit policy for the management middleware.
//
// The audit trail answers "who changed what, and who was refused". It is not a
// traffic log, and the two were conflated: every management response that was not
// 2xx produced a row, reads included. On a live deployment that meant one row
// every two seconds forever from a single panel tab polling GET /update/progress
// — denied for operators without system.status.read, and 502 wherever the updater
// sidecar is not installed. 33k rows accumulated in 18 hours, of which roughly 50
// were real events; the page that is supposed to show security-sensitive changes
// showed 656 pages of update polling.
//
// Two rules keep the trail readable:
//
//  1. An ordinary read is audited only when it was *refused*. A read that merely
//     errored is an operational fault and belongs in the request log.
//  2. Identical refusals collapse into one row per window, carrying the number of
//     occurrences folded into it, so no client can bury real events by polling.

const (
	auditResultSuccess = "success"
	auditResultDenied  = "denied"
	auditResultFailed  = "failed"
)

// managementDenial names why a request was refused.
//
// All three refusals below answer 403 and produced identical audit rows, so the
// trail could not tell "this operator lacks the permission" from "this tenant may
// not use this route at all" — two findings with completely different follow-ups
// (grant a permission vs. the operator is on the wrong tenant). The reason is
// recorded alongside the permission that was required.
type managementDenial string

const (
	denialPermission        managementDenial = "permission_denied"
	denialTenantScope       managementDenial = "tenant_resource_scope"
	denialServiceCredential managementDenial = "service_credential_forbidden"
)

const (
	// auditRepeatWindow is how long one signature holds the floor. Five minutes
	// turns a 2s polling client from ~1800 rows/hour into 12, while still showing
	// an operator that the refusals are ongoing rather than a one-off.
	auditRepeatWindow = 5 * time.Minute

	// auditRepeatIdleTTL is how long a silent signature is kept so its pending
	// count can still be attached to a later row. Beyond this the client has
	// stopped and the entry is dropped to bound memory.
	auditRepeatIdleTTL = 2 * auditRepeatWindow

	auditRepeatSweepInterval = auditRepeatWindow
)

// shouldRecordManagementAudit reports whether a management request belongs in the
// audit trail.
//
// Writes are always recorded, including failures: a rejected attempt to change
// something is exactly what an audit reader is looking for. Sensitive reads
// (request bodies, egress, exports, downloads) are recorded because reading them
// is itself a disclosure. Everything else only earns a row when it was refused.
func shouldRecordManagementAudit(write, sensitiveRead bool, result string) bool {
	if write || sensitiveRead {
		return true
	}
	return result == auditResultDenied
}

// auditRepeatKey identifies "the same thing happening again": same actor, same
// route, same outcome. Deliberately excludes the request id and timestamp.
func auditRepeatKey(tenantID, actorKind, actorUserID, action, resourceType, resourceID, result string) string {
	return strings.Join([]string{tenantID, actorKind, actorUserID, action, resourceType, resourceID, result}, "\x00")
}

type auditRepeatEntry struct {
	// windowEnds is when this signature may write again.
	windowEnds time.Time
	// lastSeen tracks liveness for sweeping, not for the window.
	lastSeen time.Time
	// suppressed counts events folded in since the last written row.
	suppressed int64
}

// auditRepeatLimiter admits one row per signature per window and counts the rest.
//
// State is per process and in-memory on purpose: it shapes this instance's own
// write rate. Losing it on restart costs at most one extra row per signature.
type auditRepeatLimiter struct {
	mu        sync.Mutex
	window    time.Duration
	idleTTL   time.Duration
	entries   map[string]*auditRepeatEntry
	nextSweep time.Time
}

func newAuditRepeatLimiter(window, idleTTL time.Duration) *auditRepeatLimiter {
	if window <= 0 {
		window = auditRepeatWindow
	}
	if idleTTL < window {
		idleTTL = 2 * window
	}
	return &auditRepeatLimiter{
		window:  window,
		idleTTL: idleTTL,
		entries: make(map[string]*auditRepeatEntry),
	}
}

// admit reports whether this event may be written and, when it may, how many
// identical events were folded into it since the previous write. A nil limiter
// admits everything so a handler built without one keeps the old behaviour.
func (l *auditRepeatLimiter) admit(key string, now time.Time) (bool, int64) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	entry, ok := l.entries[key]
	if !ok {
		l.entries[key] = &auditRepeatEntry{windowEnds: now.Add(l.window), lastSeen: now}
		return true, 0
	}
	entry.lastSeen = now
	if now.Before(entry.windowEnds) {
		entry.suppressed++
		return false, 0
	}
	folded := entry.suppressed
	entry.suppressed = 0
	entry.windowEnds = now.Add(l.window)
	return true, folded
}

// sweepLocked drops signatures that have gone quiet. An entry still holding a
// suppressed count is dropped with it: the client stopped, its first refusal is
// already in the log, and keeping the map bounded matters more than a tail count
// nobody will read.
func (l *auditRepeatLimiter) sweepLocked(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(auditRepeatSweepInterval)
	for key, entry := range l.entries {
		if now.Sub(entry.lastSeen) >= l.idleTTL {
			delete(l.entries, key)
		}
	}
}

// auditRepeatNote renders the folded count for the audit detail payload.
func auditRepeatNote(folded int64, window time.Duration) map[string]any {
	return map[string]any{
		"suppressed":     folded,
		"window_seconds": int64(window / time.Second),
		"note":           "identical events collapsed into this row: " + strconv.FormatInt(folded, 10) + " suppressed since the previous one",
	}
}

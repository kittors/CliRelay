package antigravity

import (
	"errors"
	"strings"
	"time"
)

// ErrNoProjectID is returned when onboardUser completes without a project id.
var ErrNoProjectID = errors.New("no project_id in response")

// ErrOnboardingPending is returned when onboardUser still reported the
// operation as running after the last poll. Google may finish it moments
// later, so this is a transient failure that keeps the account eligible for
// another probe — not a verdict that the account has no project.
var ErrOnboardingPending = errors.New("antigravity onboarding did not complete")

// ErrUnsupportedClient is returned when loadCodeAssist answers HTTP 200 with
// no project and an ineligible tier whose reason is UNSUPPORTED_CLIENT: Google
// did not accept the client identity that asked. Onboarding with the same
// identity cannot produce a project either, so the refusal is reported instead.
var ErrUnsupportedClient = errors.New("google rejected the antigravity client identity (UNSUPPORTED_CLIENT)")

// Metadata keys used to stop repeated project_id discovery storms.
// When Google returns onboard done without a project, retrying every
// token refresh / auth reload only burns CPU, writes auth files, and
// triggers watcher hot-reloads.
const (
	MetaProjectIDUnavailable = "antigravity_project_id_unavailable"
	MetaProjectIDProbedAt    = "antigravity_project_id_probed_at"
	MetaProjectIDProbeErr    = "antigravity_project_id_probe_err"
)

// DefaultProjectIDProbeBackoff is how long transient probe failures suppress retries.
const DefaultProjectIDProbeBackoff = 30 * time.Minute

// ProjectIDUnavailableRetryAfter is how long a "Google has no project for this
// account" verdict is trusted before discovery may run again.
//
// The verdict used to be permanent. Every account recorded that way in
// 2026-09 had been asked with the wrong client identity — loadCodeAssist
// answered UNSUPPORTED_CLIENT until the User-Agent fix in #1073 — yet kept the
// flag and was never asked again, so the corrected client could not reach it
// (#1032). Bounding the verdict keeps the storm protection, at most one
// discovery per account per window, while letting a later client, or an
// account that finished onboarding, recover on its own.
const ProjectIDUnavailableRetryAfter = 24 * time.Hour

// ShouldSkipProjectIDProbe reports whether metadata says we must not hit
// loadCodeAssist/onboardUser again for project discovery yet.
func ShouldSkipProjectIDProbe(metadata map[string]any, now time.Time, backoff time.Duration) bool {
	if metadata == nil {
		return false
	}
	if projectIDFromMetadata(metadata) != "" {
		return true
	}
	if backoff <= 0 {
		backoff = DefaultProjectIDProbeBackoff
	}
	if boolFromMetadata(metadata, MetaProjectIDUnavailable) && backoff < ProjectIDUnavailableRetryAfter {
		backoff = ProjectIDUnavailableRetryAfter
	}
	probedAt, ok := int64FromMetadata(metadata, MetaProjectIDProbedAt)
	if !ok || probedAt <= 0 {
		// Nothing says when the last probe ran. An unavailable flag without a
		// timestamp counts as expired rather than as forever.
		return false
	}
	return now.Before(time.UnixMilli(probedAt).Add(backoff))
}

// MarkProjectIDProbeFailure records a probe failure. A terminal answer (no
// project after onboarding, or the client identity refused) sets the
// unavailable flag, which holds off the next probe for
// ProjectIDUnavailableRetryAfter; any other error only starts the short
// back-off and drops a flag left by an earlier verdict.
func MarkProjectIDProbeFailure(metadata map[string]any, err error, now time.Time) {
	if metadata == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	metadata[MetaProjectIDProbedAt] = now.UnixMilli()
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg != "" {
			metadata[MetaProjectIDProbeErr] = msg
		}
	}
	if IsTerminalProjectProbeError(err) {
		metadata[MetaProjectIDUnavailable] = true
		return
	}
	// A transient failure after an expired verdict must not inherit the long
	// window: the account was not judged this time.
	delete(metadata, MetaProjectIDUnavailable)
}

// IsTerminalProjectProbeError reports whether err is Google's answer about the
// account rather than a failure to get one. The string checks cover errors
// that reached the caller already flattened to text.
func IsTerminalProjectProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNoProjectID) || errors.Is(err, ErrUnsupportedClient) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, ErrNoProjectID.Error()) || strings.Contains(msg, "UNSUPPORTED_CLIENT")
}

// ClearProjectIDProbeMarkers removes backoff/unavailable markers after a successful discovery.
func ClearProjectIDProbeMarkers(metadata map[string]any) {
	if metadata == nil {
		return
	}
	delete(metadata, MetaProjectIDUnavailable)
	delete(metadata, MetaProjectIDProbedAt)
	delete(metadata, MetaProjectIDProbeErr)
}

func projectIDFromMetadata(metadata map[string]any) string {
	raw, ok := metadata["project_id"]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func boolFromMetadata(metadata map[string]any, key string) bool {
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "y":
			return true
		}
	}
	return false
}

func int64FromMetadata(metadata map[string]any, key string) (int64, bool) {
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return 0, false
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		var n int64
		for _, ch := range trimmed {
			if ch < '0' || ch > '9' {
				return 0, false
			}
			n = n*10 + int64(ch-'0')
		}
		return n, true
	}
	return 0, false
}

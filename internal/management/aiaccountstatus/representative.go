package aiaccountstatus

import (
	"net/http"
	"strings"
	"time"

	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func isAuthUsable(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return !auth.Disabled && auth.Status != coreauth.StatusDisabled
}

// preferAuthRepresentative deterministically chooses the healthier, fresher
// credential when multiple auth aliases map to the same physical subject.
//
// ListForTenant iterates a Go map in randomized order, so representative
// selection must be a strict total order rather than keeping whichever entry
// happened to appear first; otherwise a revoked legacy alias (e.g. an older
// plan filename) can randomly overwrite the shared subject status with 401.
func preferAuthRepresentative(current, candidate *coreauth.Auth) *coreauth.Auth {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if compareAuthRepresentative(candidate, current) > 0 {
		return candidate
	}
	return current
}

// compareAuthRepresentative returns >0 if a is strictly preferable to b, <0 if
// b is preferable to a, and 0 only when both represent the same auth identity.
func compareAuthRepresentative(a, b *coreauth.Auth) int {
	if a == b {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	now := time.Now()

	if cmp := compareBool(isAuthUsable(a), isAuthUsable(b)); cmp != 0 {
		return cmp
	}
	if cmp := compareInt(authRepresentativeHealthRank(a, now), authRepresentativeHealthRank(b, now)); cmp != 0 {
		return cmp
	}
	if cmp := compareBool(!a.Unavailable, !b.Unavailable); cmp != 0 {
		return cmp
	}
	if cmp := compareTime(effectiveLastRefresh(a), effectiveLastRefresh(b)); cmp != 0 {
		return cmp
	}
	if cmp := compareTime(effectiveExpiration(a), effectiveExpiration(b)); cmp != 0 {
		return cmp
	}
	if cmp := compareTime(a.UpdatedAt, b.UpdatedAt); cmp != 0 {
		return cmp
	}
	if cmp := compareTime(a.CreatedAt, b.CreatedAt); cmp != 0 {
		return cmp
	}
	// Deterministic tie-breaker so map iteration order never affects selection.
	if a.ID != b.ID {
		if a.ID < b.ID {
			return 1
		}
		return -1
	}
	if a.Index != b.Index {
		if a.Index < b.Index {
			return 1
		}
		return -1
	}
	return 0
}

// authRepresentativeHealthRank grades credential sign-in health from worst (0)
// to best (3):
//
//	3: healthy (no auth errors, no reauth_required/refresh_pending)
//	2: refresh_pending or non-401 error
//	1: active 401 unauthorized cooldown (without confirmed reauth_required)
//	0: confirmed reauth_required / unauthorized terminal auth failure
func authRepresentativeHealthRank(auth *coreauth.Auth, now time.Time) int {
	if auth == nil {
		return 0
	}
	if codexHealthStatus := oauthHealthStatus(auth, coreauth.CodexOAuthHealthMetadataKey); codexHealthStatus != "" {
		switch codexHealthStatus {
		case "reauth_required":
			return 0
		case "refresh_pending":
			return 2
		}
	}
	if claudeHealthStatus := oauthHealthStatus(auth, coreauth.ClaudeOAuthHealthMetadataKey); claudeHealthStatus != "" {
		switch claudeHealthStatus {
		case "unauthorized":
			return 0
		case "refresh_pending":
			return 2
		}
	}

	hasUnauthorized := false
	for _, cd := range coreauth.ActiveModelCooldowns(auth, now) {
		switch cd.Reason {
		case coreauth.CooldownReasonReauthRequired:
			return 0
		case coreauth.CooldownReasonUnauthorized:
			hasUnauthorized = true
		case coreauth.CooldownReasonTokenRefreshing:
			if !hasUnauthorized {
				return 2
			}
		}
	}
	if auth.LastError != nil && !auth.Quota.Exceeded {
		switch coreauth.CooldownReasonOf(auth.LastError, auth.Quota) {
		case coreauth.CooldownReasonReauthRequired:
			return 0
		case coreauth.CooldownReasonUnauthorized:
			hasUnauthorized = true
		}
		if auth.LastError.HTTPStatus == http.StatusUnauthorized {
			hasUnauthorized = true
		}
	}
	if hasUnauthorized {
		return 1
	}
	if auth.Status == coreauth.StatusError {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth.StatusMessage)), "re-login required:") {
			return 0
		}
		return 2
	}
	return 3
}

func oauthHealthStatus(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	raw, ok := auth.Metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case map[string]any:
		status, _ := typed["status"].(string)
		return strings.ToLower(strings.TrimSpace(status))
	case map[string]string:
		return strings.ToLower(strings.TrimSpace(typed["status"]))
	default:
		return ""
	}
}

func effectiveLastRefresh(auth *coreauth.Auth) time.Time {
	if auth == nil {
		return time.Time{}
	}
	best := auth.LastRefreshedAt
	if metaTS, ok := managementauthfiles.ExtractLastRefreshTimestamp(auth.Metadata); ok && metaTS.After(best) {
		best = metaTS
	}
	return best
}

func effectiveExpiration(auth *coreauth.Auth) time.Time {
	if auth == nil {
		return time.Time{}
	}
	expiry, ok := auth.ExpirationTime()
	if !ok {
		return time.Time{}
	}
	return expiry
}

func compareBool(a, b bool) int {
	if a == b {
		return 0
	}
	if a {
		return 1
	}
	return -1
}

func compareInt(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

func compareTime(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.After(b) {
		return 1
	}
	return -1
}

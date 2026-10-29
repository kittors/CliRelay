package contentmoderation

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const ginRuntimeSnapshotKey = "content_moderation_runtime_snapshot"

// ResolutionSourceCredentialOwner is the resolution source recorded when the
// verdict came from the profile the credential's owner bound to a channel the
// requesting tenant does not own, i.e. a platform-shared credential. It takes
// the place of the channel type, which would identify the owner's configuration.
const ResolutionSourceCredentialOwner = "credential_owner"

// RuntimeSnapshot is the secret-free moderation outcome persisted with request details.
type RuntimeSnapshot struct {
	Evaluated        bool     `json:"evaluated"`
	ProfileID        string   `json:"profile_id"`
	ProfileName      string   `json:"profile_name"`
	ProfileVersion   int64    `json:"profile_version"`
	Backend          string   `json:"backend,omitempty"`
	ResolutionSource string   `json:"resolution_source"`
	ChannelType      string   `json:"channel_type"`
	ChannelID        string   `json:"channel_id"`
	Action           string   `json:"action"`
	WouldBlock       bool     `json:"would_block"`
	MatchedKeyword   string   `json:"matched_keyword,omitempty"`
	HighestCategory  string   `json:"highest_category,omitempty"`
	HighestScore     *float64 `json:"highest_score,omitempty"`
	// Safety and MatchedScanners describe a guard verdict; a guard can block
	// without an enabled category, so they are recorded independently.
	Safety          string   `json:"safety,omitempty"`
	MatchedScanners []string `json:"matched_scanners,omitempty"`
	LatencyMS       int64    `json:"latency_ms"`
	CacheHit        bool     `json:"cache_hit"`
	ErrorClass      string   `json:"error_class,omitempty"`
	ModerationError string   `json:"moderation_error,omitempty"`
}

// SetRuntimeSnapshot attaches a moderation outcome to the request's Gin context.
func SetRuntimeSnapshot(ctx context.Context, snapshot RuntimeSnapshot) {
	if ctx == nil {
		return
	}
	ginCtx, _ := ctx.Value(util.ContextKeyGin).(*gin.Context)
	if ginCtx != nil {
		ginCtx.Set(ginRuntimeSnapshotKey, snapshot)
	}
}

// RuntimeSnapshotFromGin returns the request-scoped moderation outcome, if any.
func RuntimeSnapshotFromGin(ginCtx *gin.Context) (RuntimeSnapshot, bool) {
	if ginCtx == nil {
		return RuntimeSnapshot{}, false
	}
	value, exists := ginCtx.Get(ginRuntimeSnapshotKey)
	if !exists {
		return RuntimeSnapshot{}, false
	}
	snapshot, ok := value.(RuntimeSnapshot)
	return snapshot, ok
}

func (m *RuntimeModerator) setSnapshot(ctx context.Context, tenantID string, auth *coreauth.Auth, source string, profile Profile, decision Decision, evaluated, cached bool) {
	snapshot := RuntimeSnapshot{
		Evaluated:       evaluated,
		Action:          decision.Action,
		WouldBlock:      decision.WouldBlock,
		MatchedKeyword:  decision.MatchedKeyword,
		HighestCategory: decision.HighestCategory,
		Safety:          decision.Safety,
		MatchedScanners: decision.MatchedScanners,
		LatencyMS:       decision.LatencyMS,
		CacheHit:        cached,
	}
	if credentialTenantID(auth) != tenantID {
		// The profile is the credential owner's, yet the snapshot is stored in
		// the requester's request log. The requester may learn the verdict and what
		// it matched, as for its own profiles, but not which profile, backend or
		// channel the owner configured.
		snapshot.ResolutionSource = ResolutionSourceCredentialOwner
	} else {
		channelType, channelID := resolvedChannel(auth, source)
		snapshot.ProfileID = profile.ID
		snapshot.ProfileName = profile.Name
		snapshot.ProfileVersion = profile.Version
		snapshot.Backend = profile.Backend
		snapshot.ResolutionSource = source
		snapshot.ChannelType = channelType
		snapshot.ChannelID = channelID
	}
	if decision.HighestCategory != "" {
		score := decision.HighestScore
		snapshot.HighestScore = &score
	}
	if decision.Action == ActionAPIError {
		snapshot.ErrorClass = moderationErrorClass(decision.ModerationError)
		snapshot.ModerationError = moderationErrorSummary(snapshot.ErrorClass)
	}
	SetRuntimeSnapshot(ctx, snapshot)
}

func moderationErrorSummary(errorClass string) string {
	switch errorClass {
	case "timeout":
		return "moderation API request timed out"
	case "upstream_status":
		return "moderation API returned a non-success status"
	case "invalid_response":
		return "moderation API returned an invalid response"
	case "transport":
		return "moderation API transport failed"
	case "invalid_config":
		return "moderation endpoint is misconfigured"
	default:
		return "moderation API evaluation failed"
	}
}

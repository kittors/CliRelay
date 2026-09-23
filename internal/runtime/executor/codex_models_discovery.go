package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	log "github.com/sirupsen/logrus"
)

// Manifest discovery for routing.
//
// FetchCodexModels is built for a panel: it always answers with something, falling
// back to whatever list any credential fetched last. Routing cannot use that. A
// fallback is indistinguishable from a live answer, and the process-wide cache
// behind it is not scoped to a tenant, so registering it would let one tenant's
// entitlements decide what another tenant's accounts are asked to serve.
// DiscoverCodexModels reports failure as failure, and the caller keeps the last list
// it knows to be real.

var (
	// ErrCodexManifestNeedsOAuth is returned for API-key credentials. Their models
	// endpoint belongs to whatever gateway the key points at, so it says nothing
	// about what a Codex subscription can call.
	ErrCodexManifestNeedsOAuth = errors.New("codex models: manifest discovery needs an OAuth credential")

	errCodexNotManifest          = errors.New("codex models: base URL does not serve the ChatGPT manifest")
	errCodexManifestUnrecognised = errors.New("codex models: manifest shape not recognised")
)

// DiscoverCodexModels asks the ChatGPT Codex manifest which models an OAuth
// credential can call.
func DiscoverCodexModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) ([]*sdkmodelcatalog.ModelInfo, error) {
	if codexUsesAPIKey(auth) {
		return nil, ErrCodexManifestNeedsOAuth
	}
	listing, err := requestCodexModelList(ctx, auth, cfg)
	if err != nil {
		return nil, err
	}
	if !listing.manifest {
		return nil, errCodexNotManifest
	}
	if listing.loose {
		// The heuristic walker also picks up nested objects that merely look like
		// models. That is tolerable on a panel, not in a routing table, so keep the
		// previous list until the parser learns the new shape.
		log.Warn("codex executor: models manifest no longer matches a known shape; keeping the previous routable list")
		return nil, errCodexManifestUnrecognised
	}
	// The process-wide list stays current too: the image carrier resolves from it
	// and panels fall back to it.
	storeCodexModels(listing.models)
	return listing.models, nil
}

// applyCodexManifestMetadata describes a model this build has no catalog entry for,
// using what the manifest says about it.
//
// Without it a manifest-only model registers with no reasoning support, and request
// handling strips the reasoning settings of such a model instead of forwarding them
// (see thinking.ApplyThinking). The levels are the manifest's own, filtered to what
// the Responses API accepts; when none survive, the model is marked user-defined so
// the upstream judges the settings rather than this proxy.
func applyCodexManifestMetadata(model *sdkmodelcatalog.ModelInfo, item codexModelPayload) {
	if model == nil {
		return
	}
	if description := strings.TrimSpace(item.Description); description != "" {
		model.Description = description
	}
	if window := manifestPositiveInt(item.ContextWindow); window > 0 {
		model.ContextLength = window
	}
	if levels := codexWireReasoningLevels(item.SupportedReasoningLevels); len(levels) > 0 {
		model.Thinking = &sdkmodelcatalog.ThinkingSupport{Levels: levels}
		return
	}
	model.UserDefined = true
}

// codexWireReasoningLevels extracts the reasoning efforts a manifest entry
// advertises that are also valid on the wire.
//
// The manifest lists client modes beside wire efforts — "ultra" is Codex's automatic
// task delegation, not a value the Responses API accepts — so each entry goes
// through the level parser request handling uses. Entries are {"effort": "..."}
// objects today; bare strings are accepted as well.
func codexWireReasoningLevels(raw json.RawMessage) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil
	}
	levels := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		level, ok := thinking.ParseLevelSuffix(manifestEffort(entry))
		if !ok {
			continue
		}
		value := string(level)
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		levels = append(levels, value)
	}
	return levels
}

func manifestEffort(entry json.RawMessage) string {
	var preset struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(entry, &preset); err == nil && strings.TrimSpace(preset.Effort) != "" {
		return strings.TrimSpace(preset.Effort)
	}
	var bare string
	if err := json.Unmarshal(entry, &bare); err == nil {
		return strings.TrimSpace(bare)
	}
	return ""
}

// manifestPositiveInt reads a JSON number, tolerating one sent as a string.
func manifestPositiveInt(raw json.RawMessage) int {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0
	}
	value, err := number.Int64()
	if err != nil || value <= 0 {
		return 0
	}
	return int(value)
}

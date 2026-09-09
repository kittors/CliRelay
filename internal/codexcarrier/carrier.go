// Package codexcarrier resolves the chat model that carries an image_generation
// tool call to the Codex /responses endpoint.
//
// Codex image generation has no endpoint of its own on the OAuth path: a request
// for gpt-image-2 is sent as an image_generation tool attached to an ordinary chat
// completion, so some chat model has to carry it. Two separate places picked that
// carrier, and both hardcoded the same constant:
//
//   - internal/runtime/executor, for /v1/images/generations and /v1/images/edits
//   - internal/translator/codex/openai/responses, for /v1/responses on an
//     image-only model
//
// When the hardcoded carrier left the upstream manifest, both paths began failing
// with "The '<model>' model is not supported when using Codex with a ChatGPT
// account." Neither test suite caught it, because both asserted the constant.
//
// The package sits below the executor because the executor already imports the
// translator (see translator_init.go), so the dependency cannot run the other way.
// It holds no opinion about which models exist: the executor publishes what the
// live manifest returned, and resolution simply refuses to name a carrier that is
// not on that list.
package codexcarrier

import (
	"strings"
	"sync"
)

// Default carries the tool when the manifest lists it, and when the manifest is
// not available at all. It is a preference rather than an assertion — resolution
// drops it as soon as a published manifest disagrees.
const Default = "gpt-5.5"

// excluded are manifest entries that cannot carry the tool. Spark is excluded on
// the same grounds the generation bridge already skips it; auto-review is a
// task-specific entry rather than a general chat model.
var excluded = []string{"spark", "auto-review"}

var state struct {
	mu         sync.RWMutex
	candidates []string
	override   string
}

// SetOverride records the operator's configured carrier. An empty value clears it.
//
// The override wins even when the published manifest omits it: manifests are per
// account and per client version, so a deliberate override must not be discarded
// because of a list warmed by some other credential.
func SetOverride(model string) {
	state.mu.Lock()
	state.override = strings.TrimSpace(model)
	state.mu.Unlock()
}

// Publish records the usable carriers from a live Codex manifest, in manifest
// order. Excluded and blank entries are dropped. An empty list clears what was
// published, returning resolution to Default.
func Publish(modelIDs []string) {
	candidates := make([]string, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		trimmed := strings.TrimSpace(modelID)
		if trimmed == "" || IsExcluded(trimmed) {
			continue
		}
		candidates = append(candidates, trimmed)
	}
	state.mu.Lock()
	state.candidates = candidates
	state.mu.Unlock()
}

// Resolve returns the carrier model to send upstream.
//
// Order: operator override, then Default when the manifest lists it, then the
// first usable manifest entry. That last step is what keeps image generation
// working through a model retirement without a release; it may select a costlier
// carrier than Default, which is the right trade against serving nothing.
func Resolve() string {
	state.mu.RLock()
	override := state.override
	candidates := append([]string(nil), state.candidates...)
	state.mu.RUnlock()

	if override != "" {
		return override
	}
	if len(candidates) == 0 {
		return Default
	}
	for _, candidate := range candidates {
		if strings.EqualFold(candidate, Default) {
			return candidate
		}
	}
	return candidates[0]
}

// IsExcluded reports whether a manifest entry may not carry the tool.
func IsExcluded(modelID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelID))
	if normalized == "" {
		return true
	}
	for _, name := range excluded {
		if strings.Contains(normalized, name) {
			return true
		}
	}
	return false
}

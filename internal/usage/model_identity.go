package usage

import (
	"regexp"
	"strings"
)

// SameModelIdentity reports whether two model names denote the same model.
//
// Provider aliases usually only add a routing segment: an Ollama Cloud account
// exposing "deepseek-v4-flash:0731" as "ollama/deepseek-v4-flash:0731" makes the
// executor send the unprefixed name upstream while the request log keeps the
// prefixed one. Recording that pair as "requested X, upstream Y" turns every
// aliased request into a bogus "real model ID" hint in the console, so treat a
// pure routing-prefix difference as the same model. Aliases that rename the
// model (for example "fast" -> "claude-sonnet-4") stay different and are still
// reported, because there the upstream ID carries real information.
//
// This lives in the usage package rather than in the executor because both the
// executor (when recording a row) and the management layer (when rendering one)
// must reach the same verdict; a second copy would drift.
func SameModelIdentity(requested, upstream string) bool {
	a := strings.ToLower(strings.TrimSpace(requested))
	b := strings.ToLower(strings.TrimSpace(upstream))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

// SentUpstreamModel is the model name a request actually put on the wire: the
// mapped upstream model when a mapping applied, and the requested model
// otherwise.
func SentUpstreamModel(model, upstreamModel string) string {
	if sent := strings.TrimSpace(upstreamModel); sent != "" {
		return sent
	}
	return strings.TrimSpace(model)
}

// datedModelBuildSuffix matches the release stamp upstreams append to a model
// that is otherwise the one we asked for: -2026-03-01 or -20260301.
var datedModelBuildSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

// modelReleaseKey strips the qualifiers that name a release of a model rather
// than a different model: a "-latest" pointer or a dated build. An upstream
// answering claude-sonnet-4-5-20260101 to a claude-sonnet-4-5 request resolved
// the pointer we asked it to resolve; reporting that as a reroute would flag a
// large share of normal traffic and bury the real findings.
//
// Deliberately narrow: a suffix like "-exp-a" is left alone, because there the
// upstream really did answer with a different build than the one requested.
func modelReleaseKey(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.TrimSuffix(key, "-latest")
	return datedModelBuildSuffix.ReplaceAllString(key, "")
}

// UpstreamModelMismatch reports whether an upstream answered with a model other
// than the one it was sent.
//
// It is computed on read rather than stored as a column so that a row is always
// judged by the current rule: both normalizations here are heuristics that have
// to be tuned as providers change their naming, and a stored verdict would leave
// historical rows frozen under whichever version was live when they were
// written. An empty responseModel means the upstream declared nothing — treated
// as "no finding", never as a mismatch.
func UpstreamModelMismatch(model, upstreamModel, responseModel string) bool {
	responseModel = strings.TrimSpace(responseModel)
	if responseModel == "" {
		return false
	}
	sent := SentUpstreamModel(model, upstreamModel)
	if sent == "" {
		return false
	}
	if strings.EqualFold(sent, responseModel) || SameModelIdentity(sent, responseModel) {
		return false
	}
	sentKey, responseKey := modelReleaseKey(sent), modelReleaseKey(responseModel)
	return sentKey != responseKey && !SameModelIdentity(sentKey, responseKey)
}

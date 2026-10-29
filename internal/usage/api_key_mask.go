package usage

import (
	"strings"

	"github.com/google/uuid"
)

// apiKeyMaskFiller sits between the head and the tail MaskAPIKey keeps.
const apiKeyMaskFiller = "***"

// MaskAPIKey returns the only form of a client API key that may be persisted in
// request_logs.api_key or returned by an API: a few leading and trailing
// characters around a filler. The shape is the one the panel and the public
// lookup already show, and it is idempotent, so masking an already masked value
// returns it unchanged. The key itself is identified by request_logs.api_key_id,
// never by this value; two keys may share a mask.
func MaskAPIKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Runes, not bytes: a key can be arbitrary text, and cutting a multi-byte
	// character in half would produce a string PostgreSQL refuses to store.
	runes := []rune(value)
	switch {
	case len(runes) <= 4:
		return apiKeyMaskFiller
	case len(runes) <= 10:
		return string(runes[:2]) + apiKeyMaskFiller + string(runes[len(runes)-2:])
	default:
		return string(runes[:6]) + apiKeyMaskFiller + string(runes[len(runes)-4:])
	}
}

// IsMaskedAPIKey reports whether value already has the shape MaskAPIKey produces.
func IsMaskedAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, apiKeyMaskFiller) && MaskAPIKey(value) == value
}

// requestLogInternalLabels are the non-secret origins older builds filed under
// request_logs.api_key for work that had no client key. The panel recognises the
// same set when it groups "system" calls, so they must survive masking verbatim.
var requestLogInternalLabels = map[string]struct{}{
	"antigravity": {},
	"codex":       {},
	"system":      {},
	"__system__":  {},
	"warmup":      {},
	"unknown":     {},
}

// requestLogRouteMethods prefix a route label such as "POST /auth-files/warmup".
var requestLogRouteMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD"}

// IsRequestLogLabel reports whether value is a route or an internal origin that
// the runtime stores in request_logs.api_key instead of a client secret
// ("POST /v1/chat/completions", "/healthz", "warmup"). Labels carry no secret
// and drive the "system call" classification, so they are stored as they are.
//
// This is a classification by shape and is only consulted for values that did
// not resolve to an api_keys row: a stored key is always masked, even when its
// text happens to look like a label.
func IsRequestLogLabel(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "/") {
		return true
	}
	if _, ok := requestLogInternalLabels[strings.ToLower(value)]; ok {
		return true
	}
	upper := strings.ToUpper(value)
	for _, method := range requestLogRouteMethods {
		if strings.HasPrefix(upper, method+" /") {
			return true
		}
	}
	return false
}

// storedRequestLogAPIKey returns what request_logs.api_key may hold for the
// api_key a request carried. knownKey is true when the value resolved to a
// stored key (or a key id was attached at request start): such a value is a
// secret whatever it looks like. Anything that is not a recognised label is
// masked too, because a value we cannot classify is safer hidden than shown.
func storedRequestLogAPIKey(apiKey string, knownKey bool) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return ""
	}
	if !knownKey && IsRequestLogLabel(trimmed) {
		return apiKey
	}
	return MaskAPIKey(trimmed)
}

// requestLogAPIKeyForDisplay is the read-side twin of storedRequestLogAPIKey.
// Rows written before the key was masked, or written by a build that still
// stored it, may hold a secret; this guarantees that no response does.
func requestLogAPIKeyForDisplay(stored string) string {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" {
		return ""
	}
	if IsRequestLogLabel(trimmed) || IsMaskedAPIKey(trimmed) {
		return trimmed
	}
	return MaskAPIKey(trimmed)
}

// maskedKeyLabelForFacet turns the api_key one of a key's rows holds into the
// label an unnamed key gets in the key facet: the mask its rows show. A route or
// an internal origin names no key, so it yields no label.
func maskedKeyLabelForFacet(stored string) string {
	shown := requestLogAPIKeyForDisplay(stored)
	if shown == "" || IsRequestLogLabel(shown) {
		return ""
	}
	return shown
}

// statsBucketKey names the in-memory statistics bucket of a request. A key is
// aggregated under its stable id so counters survive a secret rotation and the
// snapshot (served by /usage, exported, mirrored to Redis) never carries a
// secret. Requests that carry no key keep their route or provider label.
func statsBucketKey(apiKey, apiKeyID string) string {
	apiKey = strings.TrimSpace(apiKey)
	if id := strings.TrimSpace(apiKeyID); id != "" {
		return id
	}
	if apiKey == "" || IsRequestLogLabel(apiKey) {
		return apiKey
	}
	return MaskAPIKey(apiKey)
}

// snapshotBucketKey maps a bucket name of an imported or restored statistics
// snapshot onto the scheme of statsBucketKey. Snapshots written before buckets
// were keyed by id (the Redis mirror, older exports) are named after the client
// secret; it is resolved to the key's id here and never kept.
func snapshotBucketKey(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || IsRequestLogLabel(name) || IsMaskedAPIKey(name) {
		return name
	}
	if row := GetAPIKey(name); row != nil {
		if id := strings.TrimSpace(row.ID); id != "" {
			return id
		}
	}
	// Key ids carry no secret. Keep the id of a key that exists, and the id of one
	// that has since been deleted (ids are UUIDs) so its history stays attributable.
	if GetAPIKeyByID(name) != nil {
		return name
	}
	if _, err := uuid.Parse(name); err == nil {
		return name
	}
	// A secret that no longer resolves to a key (deleted, or never stored).
	return MaskAPIKey(name)
}

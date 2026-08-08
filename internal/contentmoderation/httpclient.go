package contentmoderation

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxModerationResponseBytes caps how much of a moderation response we read.
// Without it a faulty or hostile endpoint could stream indefinitely into the
// JSON decoder while a live request waits on the hot path.
const maxModerationResponseBytes int64 = 256 * 1024

var errResponseTooLarge = errors.New("moderation response exceeds size limit")

// normalizeModerationBaseURL validates an operator-supplied endpoint address.
//
// Credentials, query strings and fragments are rejected rather than silently
// dropped: they cannot be carried onto the resulting API path, so accepting
// them would let a profile look configured while sending something different.
// A trailing "/v1" is folded away so operators can paste either the bare host
// or the OpenAI-style base URL without producing "/v1/v1/...".
//
// Private and loopback destinations stay reachable on purpose. Guard models are
// normally self-hosted on an internal network, so blocking those addresses
// would reject the primary deployment shape; endpoint trust is an
// administrator concern enforced by who may write profiles.
func normalizeModerationBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid moderation base URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("moderation base URL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("moderation base URL must not contain credentials, query or fragment")
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return "", errors.New("invalid moderation base URL")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if strings.EqualFold(path, "/v1") {
		path = ""
	}
	parsed.Path = path
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

// moderationEndpointURL joins a validated base URL with an API path.
func moderationEndpointURL(baseURL, apiPath string) (string, error) {
	normalized, err := normalizeModerationBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	return normalized + apiPath, nil
}

// readModerationResponse enforces the response size cap. It reads one byte past
// the limit so an exactly-at-limit body is still accepted while an oversized
// one is rejected instead of being truncated into malformed JSON.
func readModerationResponse(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModerationResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read moderation response: %w", err)
	}
	if int64(len(body)) > maxModerationResponseBytes {
		return nil, errResponseTooLarge
	}
	return body, nil
}

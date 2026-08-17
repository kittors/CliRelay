package executor

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Request shaping for the MiniMax image endpoint.
//
// The public images request and the MiniMax image request agree on model, prompt
// and n, and disagree on everything else: dimensions arrive as a single "size"
// string but are sent upstream as width and height, and the byte output format is
// spelled "b64_json" on the way in and "base64" on the way out. Forwarding the body
// verbatim therefore loses the caller's size and asks for a format the endpoint
// does not accept.
//
// The allowlist mirrors the SupportedParameters declared for these models in
// internal/registry, so the catalog and the wire format cannot drift apart.

// minimaxImageRequestFields are the top-level keys the image endpoint accepts.
var minimaxImageRequestFields = map[string]struct{}{
	"model":            {},
	"prompt":           {},
	"aspect_ratio":     {},
	"width":            {},
	"height":           {},
	"response_format":  {},
	"seed":             {},
	"n":                {},
	"prompt_optimizer": {},
}

// minimaxDefaultResponseFormat is what the endpoint is asked for when the caller
// expressed no preference.
//
// The upstream default is "url", and those links expire 24 hours after the call.
// A caller that stores the response — or a console that renders it later — is then
// holding a dead link with no way to re-fetch the image. Requesting bytes has no
// expiry. An explicit caller choice is preserved, including "url".
const minimaxDefaultResponseFormat = "base64"

// shapeMiniMaxImageRequest rewrites a public images request into the MiniMax shape.
//
// It returns the shaped body and the names it dropped, so the caller can tell the
// operator which of their settings were ignored rather than silently changing the
// meaning of the request.
func shapeMiniMaxImageRequest(payload []byte) ([]byte, []string) {
	if !gjson.ParseBytes(payload).IsObject() {
		return payload, nil
	}

	// Translate before filtering: size is not an accepted field, so filtering
	// first would drop the dimensions instead of carrying them over.
	shaped := withMiniMaxDimensions(payload)
	shaped = withMiniMaxResponseFormat(shaped)

	dropped := make([]string, 0, 4)
	gjson.ParseBytes(shaped).ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if _, ok := minimaxImageRequestFields[strings.ToLower(strings.TrimSpace(name))]; ok {
			return true
		}
		if next, err := sjson.DeleteBytes(shaped, escapeJSONPathKey(name)); err == nil {
			shaped = next
			dropped = append(dropped, name)
		}
		return true
	})

	return shaped, dropped
}

// withMiniMaxDimensions converts a "size" request field into width and height.
//
// aspect_ratio takes priority upstream when both are present, so an explicit ratio
// is left to win rather than being contradicted by derived pixel values. Explicit
// width or height is likewise left alone, because that caller already speaks the
// upstream shape.
func withMiniMaxDimensions(payload []byte) []byte {
	parsed := gjson.ParseBytes(payload)
	size := strings.TrimSpace(parsed.Get("size").String())
	if size == "" {
		return payload
	}
	if strings.TrimSpace(parsed.Get("aspect_ratio").String()) != "" {
		return payload
	}
	if parsed.Get("width").Exists() || parsed.Get("height").Exists() {
		return payload
	}
	width, height, ok := parseMiniMaxImageSize(size)
	if !ok {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "width", width)
	if err != nil {
		return payload
	}
	updated, err = sjson.SetBytes(updated, "height", height)
	if err != nil {
		return payload
	}
	return updated
}

// parseMiniMaxImageSize splits a "<width>x<height>" size into its two dimensions.
// Sizes that are not a pixel pair — "auto", for one — report false and are dropped
// with the other unsupported fields.
func parseMiniMaxImageSize(size string) (int, int, bool) {
	parts := strings.SplitN(strings.ToLower(size), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// withMiniMaxResponseFormat maps the requested output format onto the two values
// the endpoint accepts, and pins a default when the caller omitted one.
func withMiniMaxResponseFormat(payload []byte) []byte {
	format := minimaxDefaultResponseFormat
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response_format").String())) {
	case "url":
		format = "url"
	case "b64_json", "base64":
		format = "base64"
	}
	updated, err := sjson.SetBytes(payload, "response_format", format)
	if err != nil {
		return payload
	}
	return updated
}

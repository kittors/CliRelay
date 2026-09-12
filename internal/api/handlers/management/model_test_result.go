package management

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util/c2pa"
	"github.com/tidwall/gjson"
)

// Probe reporting: what was sent, what came back, and what the artifact says
// about itself.
//
// A green checkmark is not a useful answer for a media model. The Codex image
// endpoint ignores the model field of the request, so gpt-image-2.5-flare,
// gpt-image-2, and a model id that does not exist all return 200 with a picture.
// An operator who only sees "ok" has learned nothing about which model served
// them. So the probe reports three things instead of one: the exact request that
// went upstream, the artifact itself, and the provenance metadata embedded in it
// — which is the only place the real generator version appears.

// modelTestResult is the modality-shaped payload the panel renders.
type modelTestResult struct {
	Kind   string                 `json:"kind"`
	Text   string                 `json:"text,omitempty"`
	Images []modelTestImageResult `json:"images,omitempty"`
	Video  *modelTestVideoResult  `json:"video,omitempty"`
}

type modelTestImageResult struct {
	B64JSON       string        `json:"b64_json,omitempty"`
	URL           string        `json:"url,omitempty"`
	RevisedPrompt string        `json:"revised_prompt,omitempty"`
	Format        string        `json:"format,omitempty"`
	Width         int           `json:"width,omitempty"`
	Height        int           `json:"height,omitempty"`
	Bytes         int           `json:"bytes,omitempty"`
	C2PA          *c2pa.Summary `json:"c2pa,omitempty"`
}

type modelTestVideoResult struct {
	URL       string  `json:"url,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
	RequestID string  `json:"request_id,omitempty"`
	Status    string  `json:"status,omitempty"`
}

// modelTestMetadata is the evidence panel: everything that helps an operator
// judge whether the run did what it claims.
type modelTestMetadataReport struct {
	Mode string `json:"mode"`
	// RequestedModel is what the operator selected; ReportedModel is what the
	// upstream response says it used. They differ more often than one would hope,
	// which is exactly why both are shown.
	RequestedModel string `json:"requested_model"`
	ReportedModel  string `json:"reported_model,omitempty"`
	Channel        string `json:"channel,omitempty"`
	Upstream       string `json:"upstream_endpoint,omitempty"`
	// ProvenanceModel is the generator the artifact's own C2PA manifest names. For
	// OpenAI images this is the field that reveals a 2.5 request served by 2.0.
	ProvenanceModel   string         `json:"provenance_model,omitempty"`
	ProvenanceVersion string         `json:"provenance_version,omitempty"`
	ProvenanceMatches *bool          `json:"provenance_matches,omitempty"`
	Usage             map[string]any `json:"usage,omitempty"`
	ResponseFields    []string       `json:"response_fields,omitempty"`
	FinishReason      string         `json:"finish_reason,omitempty"`
}

// buildModelTestResult converts one upstream response into the modality-shaped
// result plus its metadata report.
func buildModelTestResult(mode modelTestMode, model, channel string, payload []byte) (modelTestResult, modelTestMetadataReport) {
	meta := modelTestMetadataReport{
		Mode:           string(mode),
		RequestedModel: model,
		Channel:        channel,
		Upstream:       upstreamEndpointLabel(mode),
		ReportedModel:  strings.TrimSpace(gjson.GetBytes(payload, "model").String()),
		ResponseFields: topLevelFields(payload),
		FinishReason:   strings.TrimSpace(gjson.GetBytes(payload, "choices.0.finish_reason").String()),
	}
	if usage := gjson.GetBytes(payload, "usage"); usage.Exists() && usage.IsObject() {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(usage.Raw), &decoded); err == nil {
			meta.Usage = decoded
		}
	}

	switch mode {
	case modeImage, modeImageEdit:
		result := buildImageResult(payload)
		applyProvenance(&meta, model, result.Images)
		return result, meta
	case modeVideo, modeVideoFromImage:
		return buildVideoResult(payload), meta
	default:
		return modelTestResult{Kind: "text", Text: modelTestContent(payload)}, meta
	}
}

func upstreamEndpointLabel(mode modelTestMode) string {
	if alt := upstreamAlt(mode); alt != "" {
		return "/v1/" + alt
	}
	return "/v1/chat/completions"
}

func topLevelFields(payload []byte) []string {
	parsed := gjson.ParseBytes(payload)
	if !parsed.IsObject() {
		return nil
	}
	fields := make([]string, 0, 8)
	parsed.ForEach(func(key, _ gjson.Result) bool {
		fields = append(fields, key.String())
		return true
	})
	return fields
}

func buildImageResult(payload []byte) modelTestResult {
	result := modelTestResult{Kind: "image"}
	for _, item := range gjson.GetBytes(payload, "data").Array() {
		image := modelTestImageResult{
			B64JSON:       item.Get("b64_json").String(),
			URL:           item.Get("url").String(),
			RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		}
		if image.B64JSON != "" {
			if decoded, err := base64.StdEncoding.DecodeString(image.B64JSON); err == nil {
				image.Bytes = len(decoded)
				image.Format, image.Width, image.Height = describeImage(decoded)
				if summary := c2pa.Extract(decoded); summary.Present {
					image.C2PA = &summary
				}
			}
		}
		result.Images = append(result.Images, image)
	}
	return result
}

func buildVideoResult(payload []byte) modelTestResult {
	video := modelTestVideoResult{
		URL:       strings.TrimSpace(gjson.GetBytes(payload, "video.url").String()),
		Duration:  gjson.GetBytes(payload, "video.duration").Float(),
		RequestID: strings.TrimSpace(gjson.GetBytes(payload, "request_id").String()),
		Status:    strings.TrimSpace(gjson.GetBytes(payload, "status").String()),
	}
	if video.URL == "" {
		// Some providers return the clip at the top level rather than nested.
		video.URL = strings.TrimSpace(gjson.GetBytes(payload, "url").String())
	}
	return modelTestResult{Kind: "video", Video: &video}
}

// applyProvenance lifts the C2PA generator onto the metadata report and compares
// it with what was requested.
//
// The comparison is a prefix match on the generator family rather than string
// equality: the manifest says "gpt-image" where the request said
// "gpt-image-2.5-flare", so equality would flag every image as a mismatch and
// teach operators to ignore the field. What it can still catch is the case that
// matters — a manifest whose version does not line up with the requested one.
func applyProvenance(meta *modelTestMetadataReport, requested string, images []modelTestImageResult) {
	for _, image := range images {
		if image.C2PA == nil {
			continue
		}
		meta.ProvenanceModel = image.C2PA.Generator
		meta.ProvenanceVersion = image.C2PA.GeneratorVersion
		break
	}
	if meta.ProvenanceModel == "" && meta.ProvenanceVersion == "" {
		return
	}
	matches := provenanceMatchesRequest(requested, meta.ProvenanceModel, meta.ProvenanceVersion)
	meta.ProvenanceMatches = &matches
}

// provenanceMatchesRequest compares the version segment of the requested model
// id with the version the manifest claims.
//
// The version is extracted from the id rather than substring-matched against it.
// A substring test reports "gpt-image-2.5-flare" as a match for manifest version
// "2.0", because dropping the trailing ".0" leaves "2", which the id contains —
// and that is exactly the case this field exists to catch.
//
// Anything it cannot compare confidently reports a match: an unknown generator
// family or an id with no version segment would otherwise raise a warning on
// every run and teach operators to ignore it.
func provenanceMatchesRequest(requested, generator, version string) bool {
	requested = strings.ToLower(strings.TrimSpace(requested))
	generator = strings.ToLower(strings.TrimSpace(generator))
	version = strings.TrimSpace(version)
	if requested == "" || version == "" || generator == "" {
		return true
	}
	if !strings.HasPrefix(requested, generator) {
		return true
	}
	rest := strings.TrimLeft(strings.TrimPrefix(requested, generator), "-_. ")
	requestedVersion := leadingVersion(rest)
	if requestedVersion == "" {
		return true
	}
	return normalizeVersion(requestedVersion) == normalizeVersion(version)
}

// leadingVersion reads the digits-and-dots run at the head of a string, which is
// how every model id in the catalog spells its version ("2", "2.5", "1.5").
func leadingVersion(value string) string {
	end := 0
	for end < len(value) && (value[end] == '.' || (value[end] >= '0' && value[end] <= '9')) {
		end++
	}
	return strings.Trim(value[:end], ".")
}

// normalizeVersion makes "2" and "2.0" compare equal, since a model id writes the
// major-only form where a manifest writes the padded one.
func normalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	for strings.HasSuffix(value, ".0") {
		value = strings.TrimSuffix(value, ".0")
	}
	return value
}

// describeImage reads the format and pixel dimensions straight from the encoded
// bytes. Decoding the whole image to learn its size would allocate the full
// bitmap for a diagnostic line of text.
func describeImage(data []byte) (format string, width, height int) {
	if len(data) >= 24 && string(data[1:4]) == "PNG" {
		// IHDR is always the first chunk: 8-byte signature, 4-byte length,
		// 4-byte type, then width and height as big-endian uint32.
		return "png", int(binary.BigEndian.Uint32(data[16:20])), int(binary.BigEndian.Uint32(data[20:24]))
	}
	if len(data) >= 4 && data[0] == 0xFF && data[1] == 0xD8 {
		w, h := jpegDimensions(data)
		return "jpeg", w, h
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "webp", 0, 0
	}
	return "", 0, 0
}

// jpegDimensions walks the JPEG marker segments to the start-of-frame header.
func jpegDimensions(data []byte) (int, int) {
	for offset := 2; offset+9 < len(data); {
		if data[offset] != 0xFF {
			offset++
			continue
		}
		marker := data[offset+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			offset += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		// SOF0..SOF3 and SOF5..SOF15 carry the frame dimensions; the excluded
		// markers in that range are DHT (0xC4), JPG (0xC8) and DAC (0xCC).
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			return int(binary.BigEndian.Uint16(data[offset+7 : offset+9])),
				int(binary.BigEndian.Uint16(data[offset+5 : offset+7]))
		}
		if length <= 0 {
			return 0, 0
		}
		offset += 2 + length
	}
	return 0, 0
}

// maxRedactedStringLength bounds any single string kept in the request snapshot.
const maxRedactedStringLength = 200

// redactModelTestRequest produces the request snapshot the panel shows.
//
// The snapshot is the exact body sent upstream with two classes of value
// replaced: embedded media, which would be megabytes of base64 and is described
// by size instead, and anything that reads like a credential. Nothing in this
// body should carry a secret — the executor attaches the credential itself,
// downstream of here — but the snapshot is rendered verbatim in an operator's
// browser and may be pasted into a bug report, so it is filtered rather than
// trusted to be clean.
func redactModelTestRequest(payload []byte) map[string]any {
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return map[string]any{"raw": truncateForDisplay(string(payload))}
	}
	redacted, _ := redactValue(decoded).(map[string]any)
	return redacted
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if isSensitiveKey(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactValue(item))
		}
		return out
	case string:
		return redactString(typed)
	default:
		return value
	}
}

func redactString(value string) string {
	if strings.HasPrefix(value, "data:") {
		return describeDataURI(value)
	}
	return truncateForDisplay(value)
}

// describeDataURI replaces an inline upload with its media type and size, so the
// snapshot shows that an image was attached without carrying the image.
func describeDataURI(uri string) string {
	mediaType := "binary"
	if comma := strings.IndexByte(uri, ','); comma > 5 {
		header := uri[5:comma]
		if semicolon := strings.IndexByte(header, ';'); semicolon > 0 {
			header = header[:semicolon]
		}
		if header != "" {
			mediaType = header
		}
	}
	size := 0
	if decoded, err := dataURIBytes(uri); err == nil {
		size = len(decoded)
	}
	return fmt.Sprintf("[inline %s, %s]", mediaType, humanBytes(size))
}

func humanBytes(size int) string {
	switch {
	case size >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	case size >= 1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func truncateForDisplay(value string) string {
	if len(value) <= maxRedactedStringLength {
		return value
	}
	return value[:maxRedactedStringLength] + fmt.Sprintf("… (%s total)", humanBytes(len(value)))
}

// sensitiveKeyFragments are substrings that mark a field as unsafe to echo back.
var sensitiveKeyFragments = []string{
	"authorization", "api_key", "apikey", "api-key", "secret",
	"token", "password", "cookie", "credential", "session",
}

func isSensitiveKey(key string) bool {
	lowered := strings.ToLower(key)
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(lowered, fragment) {
			return true
		}
	}
	return false
}

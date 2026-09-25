// Package common holds conversion helpers shared by the protocol translators.
package common

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Tool results can carry images, and every client protocol writes them its own
// way. A translator that flattens such a result with Result.String() or
// Result.Raw sends the base64 payload upstream as text: one screenshot then
// costs millions of tokens and the request fails as "prompt is too long".
//
// Every translator reads tool results through ParseToolResult and writes them
// through one of the To* functions below, so an image is recognised the same
// way on every path and always leaves as its target's image type.

// ToolImage is one image found in a tool result. Exactly one of Data or URL is
// set.
type ToolImage struct {
	MimeType string // set with Data
	Data     string // base64 payload, without the data: prefix
	URL      string // remote http(s) URL
	Detail   string // OpenAI detail hint, when the client sent one
}

// ToolPart is one part of a tool result: text, or an image.
type ToolPart struct {
	Text  string
	Image *ToolImage
}

// ToolResult is a tool result in protocol-neutral form.
type ToolResult struct {
	// Raw holds a result that is not a content-part list: a plain string, or a
	// JSON value the tool returned as data. Parts is empty when Raw is used.
	Raw gjson.Result
	// Parts holds the parts of a content-part list.
	Parts []ToolPart
}

// HasImage reports whether the result carries at least one image.
func (r ToolResult) HasImage() bool {
	for _, p := range r.Parts {
		if p.Image != nil {
			return true
		}
	}
	return false
}

// Text joins the text parts with newlines. For a Raw result it returns the
// string value, or the JSON text of a non-string value.
func (r ToolResult) Text() string {
	if len(r.Parts) == 0 {
		if r.Raw.Type == gjson.String {
			return r.Raw.String()
		}
		return r.Raw.Raw
	}
	texts := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Image == nil && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// ParseToolResult reads a tool result value from any client protocol.
//
// A value is a content-part list when it is an array, or a single object, whose
// elements are recognised parts: text parts, or images in the OpenAI Chat,
// OpenAI Responses or Claude shape. Anything else is returned as Raw, so a tool
// that returned JSON data keeps it as data.
func ParseToolResult(value gjson.Result) ToolResult {
	switch {
	case value.IsArray():
		elements := value.Array()
		parts := make([]ToolPart, 0, len(elements))
		for _, el := range elements {
			part, ok := parseToolPart(el)
			if !ok {
				return ToolResult{Raw: value}
			}
			if part.Text != "" || part.Image != nil {
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 && len(elements) > 0 {
			return ToolResult{Raw: gjson.Parse(`""`)}
		}
		return ToolResult{Parts: parts}
	case value.IsObject():
		if part, ok := parseToolPart(value); ok && part.Image != nil {
			return ToolResult{Parts: []ToolPart{part}}
		}
	}
	return ToolResult{Raw: value}
}

// parseToolPart recognises one content part. It reports false for an element
// that is not a content part, which makes the whole value Raw data.
func parseToolPart(el gjson.Result) (ToolPart, bool) {
	if el.Type == gjson.String {
		return ToolPart{Text: el.String()}, true
	}
	if !el.IsObject() {
		return ToolPart{}, false
	}
	switch el.Get("type").String() {
	case "text", "input_text", "output_text":
		return ToolPart{Text: el.Get("text").String()}, true
	case "image_url": // OpenAI Chat
		img := imageFromURL(el.Get("image_url.url").String())
		if img != nil {
			img.Detail = el.Get("image_url.detail").String()
		}
		return ToolPart{Image: img}, true
	case "input_image": // OpenAI Responses; image_url is a string or {url}
		url := el.Get("image_url")
		if url.IsObject() {
			url = url.Get("url")
		}
		// A file_id reference carries no image data to convert. It yields an
		// empty part and is dropped rather than stringified.
		img := imageFromURL(url.String())
		if img != nil {
			img.Detail = el.Get("detail").String()
		}
		return ToolPart{Image: img}, true
	case "image": // Claude
		src := el.Get("source")
		switch src.Get("type").String() {
		case "base64":
			if data := src.Get("data").String(); data != "" {
				return ToolPart{Image: &ToolImage{MimeType: mimeOrDefault(src.Get("media_type").String()), Data: data}}, true
			}
		case "url":
			if url := src.Get("url").String(); url != "" {
				return ToolPart{Image: &ToolImage{URL: url}}, true
			}
		}
		return ToolPart{}, true
	}
	return ToolPart{}, false
}

// imageFromURL reads a data: URL or an http(s) URL. It returns nil when the URL
// carries no usable image.
func imageFromURL(url string) *ToolImage {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	if !strings.HasPrefix(url, "data:") {
		return &ToolImage{URL: url}
	}
	header, data, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || data == "" || !strings.HasSuffix(header, ";base64") {
		return nil
	}
	return &ToolImage{MimeType: mimeOrDefault(strings.TrimSuffix(header, ";base64")), Data: data}
}

func mimeOrDefault(mimeType string) string {
	if mimeType == "" {
		return "image/png"
	}
	return mimeType
}

// dataURL renders an inline image as a data: URL.
func (img ToolImage) dataURL() string {
	if img.URL != "" {
		return img.URL
	}
	return "data:" + img.MimeType + ";base64," + img.Data
}

// ToClaudeContent returns the JSON for a Claude tool_result content field: a
// string, or an array of text and image blocks when the result has parts.
func ToClaudeContent(r ToolResult) string {
	if len(r.Parts) == 0 {
		return stringJSON(r.Text())
	}
	blocks := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Image == nil {
			block, _ := sjson.Set(`{"type":"text","text":""}`, "text", p.Text)
			blocks = append(blocks, block)
			continue
		}
		if p.Image.URL != "" {
			block, _ := sjson.Set(`{"type":"image","source":{"type":"url","url":""}}`, "source.url", p.Image.URL)
			blocks = append(blocks, block)
			continue
		}
		block := `{"type":"image","source":{"type":"base64","media_type":"","data":""}}`
		block, _ = sjson.Set(block, "source.media_type", p.Image.MimeType)
		block, _ = sjson.Set(block, "source.data", p.Image.Data)
		blocks = append(blocks, block)
	}
	return "[" + strings.Join(blocks, ",") + "]"
}

// ToChatContent returns the JSON for an OpenAI Chat tool message content
// field: an array of text and image_url parts when the result carries an
// image, and a string otherwise.
func ToChatContent(r ToolResult) string {
	if !r.HasImage() {
		return stringJSON(r.Text())
	}
	parts := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Image == nil {
			part, _ := sjson.Set(`{"type":"text","text":""}`, "text", p.Text)
			parts = append(parts, part)
			continue
		}
		part, _ := sjson.Set(`{"type":"image_url","image_url":{"url":""}}`, "image_url.url", p.Image.dataURL())
		if p.Image.Detail != "" {
			part, _ = sjson.Set(part, "image_url.detail", p.Image.Detail)
		}
		parts = append(parts, part)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ToResponsesOutput returns the JSON for an OpenAI Responses
// function_call_output.output field: an array of input_text and input_image
// parts when the result carries an image, and a string otherwise.
func ToResponsesOutput(r ToolResult) string {
	if !r.HasImage() {
		return stringJSON(r.Text())
	}
	parts := make([]string, 0, len(r.Parts))
	for _, p := range r.Parts {
		if p.Image == nil {
			part, _ := sjson.Set(`{"type":"input_text","text":""}`, "text", p.Text)
			parts = append(parts, part)
			continue
		}
		part, _ := sjson.Set(`{"type":"input_image","image_url":""}`, "image_url", p.Image.dataURL())
		if p.Image.Detail != "" {
			part, _ = sjson.Set(part, "detail", p.Image.Detail)
		}
		parts = append(parts, part)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// SetGeminiFunctionResponse fills response.result and parts on a Gemini
// functionResponse object from a tool result and returns the object.
//
// Gemini reads images only from functionResponse.parts, so they go there as
// inlineData (base64) or fileData (URL), and response.result keeps the text.
// A result without images keeps the translator's existing behaviour:
// structured JSON stays structured and anything else becomes a string.
func SetGeminiFunctionResponse(functionResponse string, r ToolResult) string {
	if !r.HasImage() {
		switch {
		case len(r.Parts) > 0:
			functionResponse, _ = sjson.Set(functionResponse, "response.result", r.Text())
		case r.Raw.IsObject() || r.Raw.IsArray():
			functionResponse, _ = sjson.SetRaw(functionResponse, "response.result", r.Raw.Raw)
		default:
			functionResponse, _ = sjson.Set(functionResponse, "response.result", r.Text())
		}
		return functionResponse
	}
	functionResponse, _ = sjson.Set(functionResponse, "response.result", r.Text())
	for _, p := range r.Parts {
		if p.Image == nil {
			continue
		}
		var part string
		if p.Image.URL != "" {
			part = `{"fileData":{"mimeType":"","fileUri":""}}`
			part, _ = sjson.Set(part, "fileData.mimeType", mimeFromURL(p.Image.URL))
			part, _ = sjson.Set(part, "fileData.fileUri", p.Image.URL)
		} else {
			part = `{"inlineData":{"mimeType":"","data":""}}`
			part, _ = sjson.Set(part, "inlineData.mimeType", p.Image.MimeType)
			part, _ = sjson.Set(part, "inlineData.data", p.Image.Data)
		}
		functionResponse, _ = sjson.SetRaw(functionResponse, "parts.-1", part)
	}
	return functionResponse
}

// mimeFromURL guesses an image MIME type from a URL's extension; Gemini
// requires one on fileData.
func mimeFromURL(url string) string {
	path, _, _ := strings.Cut(strings.ToLower(url), "?")
	switch {
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	case strings.HasSuffix(path, ".webp"):
		return "image/webp"
	}
	return "image/png"
}

func stringJSON(s string) string {
	out, _ := sjson.Set(`{"v":""}`, "v", s)
	return gjson.Get(out, "v").Raw
}

package executor

import (
	"bytes"
	"sort"
	"strings"
	"unsafe"

	"github.com/tidwall/gjson"
)

// The scan half of the request shape (codex_request_shape.go): one pass over a
// /responses request body that counts what is in it and keeps none of it.
//
// Everything that reaches the shape is a number, a boolean, or a name that passed
// a fixed alphabet. The body is the client's: type names, roles and even the
// top-level keys are whatever the caller wrote, so a name is only ever kept when
// it fits ^[a-z_:]{1,40}$ (codexShapeName) or sits in a fixed list, and the rest
// is counted as "other". No text, id, argument, call id, image or ciphertext
// value is copied out; ciphertext and images are only measured.

const (
	// codexShapeMaxTypes bounds how many distinct type names one counter keeps, so
	// a caller cannot grow the shape by inventing types.
	codexShapeMaxTypes = 24
	// codexShapeMaxNameBytes is the longest name a counter keeps.
	codexShapeMaxNameBytes = 40
	// codexShapeOther collects every name the counters refuse.
	codexShapeOther = "other"
)

var (
	codexDataImageMarker = []byte("data:image/")

	// codexShapeLiteMetadataKey is where a Responses Lite client repeats its
	// header inside client_metadata.
	codexShapeLiteMetadataKey = strings.TrimPrefix(codexResponsesLiteMetadata, "client_metadata.")

	// codexShapeKnownKeys is the fixed set of top-level request keys the shape
	// names. A key outside it is counted, never copied.
	codexShapeKnownKeys = map[string]struct{}{
		"background": {}, "client_metadata": {}, "context_management": {}, "conversation": {},
		"include": {}, "input": {}, "instructions": {}, "max_output_tokens": {}, "metadata": {},
		"model": {}, "parallel_tool_calls": {}, "previous_response_id": {}, "prompt_cache_key": {},
		"prompt_cache_retention": {}, "reasoning": {}, "safety_identifier": {}, "service_tier": {},
		"store": {}, "stream": {}, "stream_options": {}, "temperature": {}, "text": {},
		"tool_choice": {}, "tools": {}, "top_p": {}, "truncation": {}, "user": {},
	}
)

// codexShapeName returns s when it is a type-like name (^[a-z_:]{1,40}$) and
// "other" otherwise.
func codexShapeName(s string) string {
	if s == "" || len(s) > codexShapeMaxNameBytes {
		return codexShapeOther
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && c != '_' && c != ':' {
			return codexShapeOther
		}
	}
	return s
}

// codexTypeCounter counts occurrences by bucket name. Names were already passed
// through codexShapeName; the counter adds the cap on how many it keeps.
//
// The names it is given may point into the request body (see scanBody), and a
// map keeps its key alive. So a name is cloned before it becomes a key, and the
// counts are pointers: updating a count through map[name]++ would store the
// caller's string as the key again, since Go overwrites the key of an existing
// entry on assignment.
type codexTypeCounter struct {
	counts map[string]*int
	other  int
}

func (c *codexTypeCounter) add(bucket string) {
	if n := c.counts[bucket]; n != nil {
		*n++
		return
	}
	if len(c.counts) >= codexShapeMaxTypes {
		c.other++
		return
	}
	if c.counts == nil {
		c.counts = make(map[string]*int, 8)
	}
	n := 1
	c.counts[strings.Clone(bucket)] = &n
}

func (c *codexTypeCounter) result() map[string]int {
	if len(c.counts) == 0 && c.other == 0 {
		return nil
	}
	out := make(map[string]int, len(c.counts)+1)
	for name, n := range c.counts {
		out[name] = *n
	}
	if c.other > 0 {
		out[codexShapeOther] += c.other
	}
	return out
}

// codexInputBucket names an input item for the by-type counts: its type, with
// messages split by role. An item with a role and no type is a message, which
// is what the Responses API reads it as.
func codexInputBucket(typ, role string) string {
	if typ == "" && role != "" {
		typ = "message"
	}
	if typ != "message" {
		return codexShapeName(typ)
	}
	switch role {
	case "user":
		return "message:user"
	case "assistant":
		return "message:assistant"
	case "system":
		return "message:system"
	case "developer":
		return "message:developer"
	}
	return "message:other"
}

// codexObjectString reads one string field of a JSON object, stopping at it.
func codexObjectString(obj gjson.Result, name string) string {
	if !obj.IsObject() {
		return ""
	}
	found := ""
	obj.ForEach(func(key, value gjson.Result) bool {
		if key.Str != name {
			return true
		}
		if value.Type == gjson.String {
			found = value.Str
		}
		return false
	})
	return found
}

// scanBody fills the shape's body-derived fields in one pass over body.
//
// body is read through a string view instead of being copied: it is the request
// the executor still holds, over a megabyte more often than not and tens of
// megabytes at times, and a copy would double the memory a failing request holds
// while the scan runs. The view never escapes this function and body is not
// written while it runs (it is the plain body of a request already in flight).
// Nothing the scan keeps points into it: names are cloned and all else is
// numbers.
//
// input is walked once with ForEach, element by element, never with a path
// lookup per item: a path lookup scans the request from its start each time.
func (s *codexUpstreamShape) scanBody(body []byte) {
	s.Bytes.Plain = len(body)
	if len(body) == 0 {
		return
	}
	view := unsafe.String(unsafe.SliceData(body), len(body))
	gjson.Parse(view).ForEach(func(key, value gjson.Result) bool {
		name := key.Str
		s.noteTopLevelKey(name)
		switch name {
		case "input":
			s.scanInput(value)
		case "tools":
			s.scanTools(value)
		case "instructions":
			if value.Type == gjson.String {
				s.Request.InstructionsBytes = len(value.Raw) - 2
			}
		case "store":
			if value.Type == gjson.True || value.Type == gjson.False {
				stored := value.Type == gjson.True
				s.Request.Store = &stored
			}
		case "stream":
			if value.Type == gjson.True || value.Type == gjson.False {
				streamed := value.Type == gjson.True
				s.Request.Stream = &streamed
			}
		case "reasoning":
			// The bridge's own test: an effort is only there when it is a non-empty string.
			effort := codexObjectString(value, "effort")
			s.Request.ReasoningEffort = strings.TrimSpace(effort) != ""
		case "tool_choice":
			switch {
			case value.Type == gjson.String:
				s.Request.ToolChoice = strings.Clone(codexShapeName(value.Str))
			case value.IsObject():
				s.Request.ToolChoice = strings.Clone(codexShapeName(codexObjectString(value, "type")))
			}
		case "client_metadata":
			if value.IsObject() {
				value.ForEach(func(k, v gjson.Result) bool {
					if k.Str == codexShapeLiteMetadataKey {
						s.lite = v.Type == gjson.True || (v.Type == gjson.String && strings.EqualFold(strings.TrimSpace(v.Str), "true"))
						return false
					}
					return true
				})
			}
		}
		return true
	})
	// data:image/ URLs are counted over the whole body in one pass of the bytes
	// package, then the structured ones (input_image parts) are taken off: what
	// is left sits inside text, which is how Desktop replays images it was shown
	// as markdown.
	s.Images.DataURLInText = max(0, bytes.Count(body, codexDataImageMarker)-s.dataURLImages)
	s.Input.ByType = s.inputTypes.result()
	s.Tools.ByType = s.toolTypes.result()
	sort.Strings(s.Request.Keys)
}

func (s *codexUpstreamShape) noteTopLevelKey(name string) {
	if _, known := codexShapeKnownKeys[name]; !known {
		s.Request.OtherKeys++
		return
	}
	s.Request.Keys = append(s.Request.Keys, strings.Clone(name))
}

func (s *codexUpstreamShape) scanTools(tools gjson.Result) {
	if !tools.IsArray() {
		return
	}
	tools.ForEach(func(_, tool gjson.Result) bool {
		s.Tools.TopLevel++
		s.toolTypes.add(codexShapeName(codexObjectString(tool, "type")))
		return true
	})
}

func (s *codexUpstreamShape) scanInput(input gjson.Result) {
	if input.Type == gjson.String {
		s.Input.StringBytes = len(input.Raw) - 2
		return
	}
	if !input.IsArray() {
		return
	}
	s.Input.Bytes = len(input.Raw)
	input.ForEach(func(_, item gjson.Result) bool {
		s.Input.Items++
		s.scanInputItem(item)
		return true
	})
}

// scanInputItem reads one input item's top-level keys once and keeps only what
// the shape counts. The large values (text, arguments, outputs, ciphertext) are
// stepped over, not read.
func (s *codexUpstreamShape) scanInputItem(item gjson.Result) {
	var typ, role string
	var content, output, nested gjson.Result
	cipherBytes, hasCipher := 0, false
	if item.IsObject() {
		item.ForEach(func(key, value gjson.Result) bool {
			switch key.Str {
			case "type":
				if value.Type == gjson.String {
					typ = value.Str
				}
			case "role":
				if value.Type == gjson.String {
					role = value.Str
				}
			case "encrypted_content":
				if value.Type == gjson.String {
					hasCipher, cipherBytes = true, len(value.Raw)-2
				}
			case "content":
				content = value
			case "output":
				output = value
			case "tools":
				nested = value
			}
			return true
		})
	}

	bucket := codexInputBucket(typ, role)
	s.inputTypes.add(bucket)
	if size := len(item.Raw); size > s.Input.LargestItemBytes {
		s.Input.LargestItemBytes, s.Input.LargestItemType = size, strings.Clone(bucket)
	}
	if hasCipher {
		s.noteCiphertext(typ, cipherBytes)
	}
	// Responses Lite carries its tools in an additional_tools item instead of the
	// top-level tools array.
	if typ == "additional_tools" {
		s.Tools.AdditionalToolsItems++
		if nested.IsArray() {
			nested.ForEach(func(_, _ gjson.Result) bool {
				s.Tools.AdditionalToolsTools++
				return true
			})
		}
	}
	// Images sit in a message's content and in a tool call's output.
	s.scanImageParts(content)
	s.scanImageParts(output)
}

func (s *codexUpstreamShape) noteCiphertext(typ string, size int) {
	kind := codexShapeOther
	switch {
	case typ == "reasoning":
		kind = "reasoning"
	case strings.HasPrefix(typ, "compaction"):
		kind = "compaction"
	}
	if s.Ciphertext == nil {
		s.Ciphertext = make(map[string]codexShapeCipher, 3)
	}
	cipher := s.Ciphertext[kind]
	cipher.N++
	cipher.Bytes += size
	s.Ciphertext[kind] = cipher
}

// scanImageParts counts the input_image parts of a content or output array and
// measures their image_url.
func (s *codexUpstreamShape) scanImageParts(parts gjson.Result) {
	if !parts.IsArray() {
		return
	}
	parts.ForEach(func(_, part gjson.Result) bool {
		// Cheap filter before reading the part's keys: most parts are text, and
		// reading a large text value costs more than looking for the type name.
		if !strings.Contains(part.Raw, `"input_image"`) || !part.IsObject() {
			return true
		}
		isImage := false
		var url gjson.Result
		part.ForEach(func(key, value gjson.Result) bool {
			switch key.Str {
			case "type":
				isImage = value.Type == gjson.String && value.Str == "input_image"
			case "image_url":
				url = value
			}
			return true
		})
		if !isImage {
			return true
		}
		s.Images.InputImage++
		if url.Type != gjson.String {
			return true
		}
		size := len(url.Raw) - 2
		s.Images.InputImageBytes += size
		s.Images.LargestBytes = max(s.Images.LargestBytes, size)
		if strings.HasPrefix(url.Str, "data:") {
			s.dataURLImages++
		}
		return true
	})
}

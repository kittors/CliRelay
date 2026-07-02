package vision

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ImagePart describes a single image content part found in a payload.
type ImagePart struct {
	ArrayName string // "messages" or "input"
	MsgIdx    int    // index in the array
	PartIdx   int    // index in the content array
	Path      string // path to the image part or data URL string
	Type      string // original type ("image_url" / "input_image" / "image")
	Data      string // base64 data (may be empty for remote URLs)
	RemoteURL string // remote URL if not inline
	MIMEType  string
	IsCurrent bool // true if this part belongs to the last user message
}

// WalkResult contains all image parts found and whether any are current/historical.
type WalkResult struct {
	Parts          []ImagePart
	CurrentImages  bool
	HistoricalOnly bool // true when only non-last-message images exist
	MessageCount   int
	TurnIndex      int // approximate turn index from message count
}

// WalkPayload scans a Chat Completions or Responses API payload for image
// content parts in user messages. It returns structured info about each image
// without modifying the payload.
func WalkPayload(payload []byte) *WalkResult {
	result := &WalkResult{}

	// Try Chat Completions format (messages array)
	msgs := gjson.GetBytes(payload, "messages")
	if msgs.Exists() && msgs.IsArray() {
		result.MessageCount = len(msgs.Array())
		result.TurnIndex = result.MessageCount
		result.walkArray(payload, "messages", msgs.Array())
		return result
	}

	// Try Responses API format (input array)
	input := gjson.GetBytes(payload, "input")
	if input.Exists() && input.IsArray() {
		result.MessageCount = len(input.Array())
		result.TurnIndex = result.MessageCount
		result.walkArray(payload, "input", input.Array())
		return result
	}

	return result
}

func (r *WalkResult) walkArray(payload []byte, arrayName string, items []gjson.Result) {
	lastUserIdx := -1

	// Scan backwards to find the last user message index
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}

	for i, item := range items {
		if item.Get("role").String() != "user" {
			if arrayName == "input" && isFunctionOutputItem(item) {
				r.walkFunctionOutputItem(payload, arrayName, i, item, i > lastUserIdx)
			}
			continue
		}
		isCurrent := (i == lastUserIdx)
		r.walkMessage(payload, arrayName, i, item, isCurrent)
	}

	r.CurrentImages = false
	r.HistoricalOnly = true
	for _, p := range r.Parts {
		if p.IsCurrent {
			r.CurrentImages = true
			r.HistoricalOnly = false
			break
		}
	}
}

func (r *WalkResult) walkMessage(payload []byte, arrayName string, msgIdx int, msg gjson.Result, isCurrent bool) {
	content := msg.Get("content")
	if !content.Exists() {
		return
	}

	// String content — only check for data:image prefix
	if content.IsArray() {
		for pIdx, part := range content.Array() {
			r.walkContentPart(payload, arrayName, msgIdx, pIdx, part, isCurrent)
		}
	} else {
		text := content.String()
		if hasDataImagePrefix(text) {
			r.Parts = append(r.Parts, ImagePart{
				ArrayName: arrayName,
				MsgIdx:    msgIdx,
				PartIdx:   0,
				Path:      arrayName + "." + indexStr(msgIdx) + ".content",
				Type:      "text",
				Data:      text,
				IsCurrent: isCurrent,
			})
		}
	}
}

func (r *WalkResult) walkContentPart(payload []byte, arrayName string, msgIdx, pIdx int, part gjson.Result, isCurrent bool) {
	r.walkContentPartAtPath(payload, arrayName, msgIdx, pIdx, part, isCurrent, arrayName+"."+indexStr(msgIdx)+".content."+indexStr(pIdx))
}

func (r *WalkResult) walkContentPartAtPath(payload []byte, arrayName string, msgIdx, pIdx int, part gjson.Result, isCurrent bool, path string) {
	partType := part.Get("type").String()
	if !isImageType(partType) && !part.Get("image_url").Exists() {
		return
	}

	ip := ImagePart{
		ArrayName: arrayName,
		MsgIdx:    msgIdx,
		PartIdx:   pIdx,
		Path:      path,
		Type:      partType,
		IsCurrent: isCurrent,
	}

	// Extract data from various formats
	// Format 1: {"image_url": {"url": "data:..."}}
	if imgURL := part.Get("image_url"); imgURL.Exists() {
		if imgURL.IsObject() {
			url := imgURL.Get("url").String()
			ip.Data, ip.MIMEType = parseDataURL(url)
			if ip.Data == "" {
				ip.RemoteURL = url
			}
		} else if imgURL.Type == gjson.String {
			url := imgURL.String()
			ip.Data, ip.MIMEType = parseDataURL(url)
			if ip.Data == "" {
				ip.RemoteURL = url
			}
		}
	}

	// Format 2: {"source": {"data": "...", "media_type": "..."}}
	if source := part.Get("source"); source.Exists() && source.IsObject() {
		if data := source.Get("data").String(); data != "" {
			ip.Data = data
			ip.MIMEType = source.Get("media_type").String()
		}
	}

	r.Parts = append(r.Parts, ip)
}

func (r *WalkResult) walkFunctionOutputItem(payload []byte, arrayName string, msgIdx int, item gjson.Result, isCurrent bool) {
	for _, field := range []string{"output", "content"} {
		value := item.Get(field)
		if !value.Exists() {
			continue
		}
		r.walkFunctionOutputValue(payload, arrayName, msgIdx, value, isCurrent, arrayName+"."+indexStr(msgIdx)+"."+field)
	}
}

func (r *WalkResult) walkFunctionOutputValue(payload []byte, arrayName string, msgIdx int, value gjson.Result, isCurrent bool, path string) {
	if value.IsArray() {
		for i, child := range value.Array() {
			r.walkFunctionOutputValue(payload, arrayName, msgIdx, child, isCurrent, path+"."+indexStr(i))
		}
		return
	}
	if value.IsObject() {
		partType := value.Get("type").String()
		if isImageType(partType) || value.Get("image_url").Exists() {
			r.walkContentPartAtPath(payload, arrayName, msgIdx, 0, value, isCurrent, path)
			return
		}
		for key, child := range value.Map() {
			r.walkFunctionOutputValue(payload, arrayName, msgIdx, child, isCurrent, path+"."+key)
		}
		return
	}
	text := value.String()
	if hasDataImagePrefix(text) {
		r.Parts = append(r.Parts, ImagePart{
			ArrayName: arrayName,
			MsgIdx:    msgIdx,
			PartIdx:   0,
			Path:      path,
			Type:      "text",
			Data:      text,
			IsCurrent: isCurrent,
		})
	}
}

func isFunctionOutputItem(item gjson.Result) bool {
	switch item.Get("type").String() {
	case "function_call_output", "computer_call_output":
		return true
	}
	return false
}

// ReplaceImagePart replaces an image content part with a text placeholder,
// using the array type to determine the correct content type field name.
func ReplaceImagePart(payload []byte, ip ImagePart, placeholderText string) ([]byte, error) {
	return ReplaceImagePartEx(payload, ip, placeholderText, ip.ArrayName)
}

// ReplaceImagePartEx replaces an image content part with a text placeholder.
// arrayType controls the content type: "messages" → "text", "input" → "input_text".
func ReplaceImagePartEx(payload []byte, ip ImagePart, placeholderText string, arrayType string) ([]byte, error) {
	contentType := "text"
	if arrayType == "input" {
		contentType = "input_text"
	}
	contentField := "text" // Responses API input_text uses "text" as field name

	dataPath := ip.ArrayName + "." + indexStr(ip.MsgIdx) + ".content." + indexStr(ip.PartIdx)
	if ip.Path != "" {
		dataPath = ip.Path
	}

	current := gjson.GetBytes(payload, dataPath)
	if current.Exists() && current.Type == gjson.String {
		return sjson.SetBytes(payload, dataPath, placeholderText)
	}

	payload, err := sjson.SetBytes(payload, dataPath+".type", contentType)
	if err != nil {
		return payload, err
	}
	payload, err = sjson.SetBytes(payload, dataPath+"."+contentField, placeholderText)
	if err != nil {
		return payload, err
	}
	payload, err = sjson.DeleteBytes(payload, dataPath+".image_url")
	if err != nil {
		return payload, err
	}
	payload, err = sjson.DeleteBytes(payload, dataPath+".source")
	if err != nil {
		return payload, err
	}
	payload, err = sjson.DeleteBytes(payload, dataPath+".detail")
	if err != nil {
		return payload, err
	}
	return payload, nil
}

// InjectRegistryNote appends a synthetic text content part to the last user
// message with image registry information.
func InjectRegistryNote(payload []byte, note string) ([]byte, error) {
	arrayType := detectArrayType(payload)
	return InjectRegistryNoteEx(payload, note, arrayType)
}

// InjectRegistryNoteEx appends a registry note with the correct content type
// for the given array type ("messages" → type="text", "input" → type="input_text").
// The Responses API input_text parts use "text" as the data field name.
func InjectRegistryNoteEx(payload []byte, note string, arrayType string) ([]byte, error) {
	contentType := "text"
	if arrayType == "input" {
		contentType = "input_text"
	}

	// Determine format
	arrayName := "messages"
	items := gjson.GetBytes(payload, "messages")
	if !items.Exists() || !items.IsArray() {
		arrayName = "input"
		items = gjson.GetBytes(payload, "input")
		if !items.Exists() || !items.IsArray() {
			return payload, nil
		}
	}

	// Find last user message index
	lastUserIdx := -1
	for i := len(items.Array()) - 1; i >= 0; i-- {
		if items.Array()[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return payload, nil
	}

	// Get the message content
	msg := items.Array()[lastUserIdx]
	content := msg.Get("content")
	if !content.Exists() || (!content.IsArray() && content.Type != gjson.String) {
		return payload, nil
	}

	contentPath := arrayName + "." + indexStr(lastUserIdx) + ".content"

	// If content is a string, convert to array first
	if content.Type == gjson.String {
		existingText := content.String()
		payload, _ = sjson.SetBytes(payload, contentPath, []any{
			map[string]any{"type": contentType, "text": existingText},
			map[string]any{"type": contentType, "text": note},
		})
		return payload, nil
	}

	// Array — find the next index
	parts := content.Array()
	nextIdx := len(parts)

	notePath := contentPath + "." + indexStr(nextIdx)
	payload, err := sjson.SetBytes(payload, notePath+".type", contentType)
	if err != nil {
		return payload, err
	}
	payload, err = sjson.SetBytes(payload, notePath+".text", note)
	return payload, err
}

func isImageType(t string) bool {
	switch t {
	case "image_url", "input_image", "image":
		return true
	}
	return false
}

func hasDataImagePrefix(s string) bool {
	return len(s) > 20 && (s[:11] == "data:image/" || s[:11] == "data:image;")
}

func parseDataURL(url string) (data, mediaType string) {
	if len(url) < 20 || url[:5] != "data:" {
		return "", ""
	}
	parts := split2(url[5:], ";base64,")
	if len(parts) == 2 {
		return parts[1], parts[0]
	}
	return "", ""
}

func split2(s, sep string) []string {
	idx := indexAfterPrefix(s, sep)
	if idx < 0 {
		return nil
	}
	return []string{s[:idx], s[idx+len(sep):]}
}

func indexAfterPrefix(s, sep string) int {
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

func indexStr(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	// For simplicity with gjson/sjson paths, use simple formatting
	if i < 100 {
		return string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	// Fallback for larger indices (unlikely in practice)
	return fmtInt(i)
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 4)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

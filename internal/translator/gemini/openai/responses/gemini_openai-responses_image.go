package responses

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// geminiInlineImage is one generated image lifted out of a Gemini part.
//
// Image models answer with inline bytes rather than text, and a response that
// carries nothing else used to translate into an empty `output` array: the
// caller saw a completed response with usage but no content at all.
type geminiInlineImage struct {
	Data     string
	MimeType string
}

// geminiInlineImageFromPart reads a part's inline image, accepting both the
// camelCase and snake_case spellings the upstreams mix.
func geminiInlineImageFromPart(part gjson.Result) (geminiInlineImage, bool) {
	inline := part.Get("inlineData")
	if !inline.Exists() {
		inline = part.Get("inline_data")
	}
	data := inline.Get("data").String()
	if data == "" {
		return geminiInlineImage{}, false
	}
	mimeType := inline.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inline.Get("mime_type").String()
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return geminiInlineImage{Data: data, MimeType: mimeType}, true
}

// responsesImageOutputFormat maps a mime type onto the short format name the
// Responses API reports on an image_generation_call.
func responsesImageOutputFormat(mimeType string) string {
	format := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/")
	if idx := strings.IndexAny(format, ";+"); idx >= 0 {
		format = format[:idx]
	}
	switch format {
	case "":
		return "png"
	case "jpg":
		return "jpeg"
	default:
		return format
	}
}

// responsesImageItemID names an image output item after its response and slot,
// matching how message and reasoning items are named in this package.
func responsesImageItemID(responseID string, index int) string {
	return fmt.Sprintf("ig_%s_%d", strings.TrimPrefix(responseID, "resp_"), index)
}

// responsesImageItemJSON builds one image_generation_call output item. The
// bytes ride in `result` as bare base64, which is where the Responses API puts
// a generated image.
func responsesImageItemJSON(itemID, status string, image geminiInlineImage, withResult bool) string {
	item := `{"id":"","type":"image_generation_call","status":"","output_format":"","result":null}`
	item, _ = sjson.Set(item, "id", itemID)
	item, _ = sjson.Set(item, "status", status)
	item, _ = sjson.Set(item, "output_format", responsesImageOutputFormat(image.MimeType))
	if withResult {
		item, _ = sjson.Set(item, "result", image.Data)
	}
	return item
}

// emitResponsesImageEvents streams one generated image as its own output item
// and records it for the aggregated response.completed payload. The protocol
// has no incremental form for image bytes, so the item opens and closes within
// the same chunk.
func emitResponsesImageEvents(st *geminiToResponsesState, nextSeq func() int, image geminiInlineImage) []string {
	index := st.NextIndex
	st.NextIndex++
	itemID := responsesImageItemID(st.ResponseID, index)
	if st.ImageItems == nil {
		st.ImageItems = make(map[int]geminiInlineImage)
	}
	st.ImageItems[index] = image

	events := make([]string, 0, 3)

	added := `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{}}`
	added, _ = sjson.Set(added, "sequence_number", nextSeq())
	added, _ = sjson.Set(added, "output_index", index)
	added, _ = sjson.SetRaw(added, "item", responsesImageItemJSON(itemID, "in_progress", image, false))
	events = append(events, string(emitEvent("response.output_item.added", []byte(added))))

	generated := `{"type":"response.image_generation_call.completed","sequence_number":0,"output_index":0,"item_id":""}`
	generated, _ = sjson.Set(generated, "sequence_number", nextSeq())
	generated, _ = sjson.Set(generated, "output_index", index)
	generated, _ = sjson.Set(generated, "item_id", itemID)
	events = append(events, string(emitEvent("response.image_generation_call.completed", []byte(generated))))

	done := `{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{}}`
	done, _ = sjson.Set(done, "sequence_number", nextSeq())
	done, _ = sjson.Set(done, "output_index", index)
	done, _ = sjson.SetRaw(done, "item", responsesImageItemJSON(itemID, "completed", image, true))
	events = append(events, string(emitEvent("response.output_item.done", []byte(done))))

	return events
}

package executor

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	// Inbound history hygiene: Desktop re-sends prior assistant text that may contain multi-MB
	// data:image URLs from our outbound display rewrite. Strip pixels, keep a short placeholder
	// so the model still knows an image existed — zero server-side image storage.
	codexMarkdownDataURLImage = regexp.MustCompile(`!\[([^\]]*)\]\((data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=\r\n]{256,})\)`)
	codexBareDataURLImage     = regexp.MustCompile(`data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=\r\n]{256,}`)
)

// codexHistoryImage is a single image extracted from the current request body only.
// Never written to disk or process-global cache — lives only for this sanitize call.
type codexHistoryImage struct {
	MIME   string
	Result string
}

// before upstream. Desktop keeps outbound data-url markdown for display, then re-sends it
// on the next turn as text — that blows the model context (context_length_exceeded).
func stripCodexHistoryDataURLImages(body []byte) []byte {
	store := gjson.GetBytes(body, "store")
	return stripCodexHistoryDataURLImagesForStore(body, store.Exists() && !store.Bool())
}

// stripCodexHistoryDataURLImagesForStore also strips server-issued history item IDs when
// store is disabled. Those IDs otherwise make upstream attempt to resolve items that were
// never persisted, while the remaining inline payload is sufficient for stateless replay.
//
// Strategy (no server storage / no global cache):
//  1. Replace huge data URLs in history text with short cliproxy-image:N placeholders
//  2. Re-attach at most the last extracted image as a structured input_image on the latest
//     user turn so the model can re-identify / edit without keeping base64-as-text history
//  3. With store=false, strip top-level item IDs and drop reference-only history shells
//  4. All image bytes come from the current request body and die with the request
//
// Implementation rebuilds the input array once. Never call sjson.Set/Delete on the full
// multi-MB body for each history item — that was the production RSS spike path.
func stripCodexHistoryDataURLImagesForStore(body []byte, stripStoredItemIDs bool) []byte {
	if len(body) == 0 {
		return body
	}
	// Fast path: nothing to do when payload has neither markdown data URLs nor a large
	// image_generation_call.result nor stored history item IDs.
	hasDataURL := bytes.Contains(body, []byte("data:image/"))
	hasIGResult := bytes.Contains(body, []byte(`"image_generation_call"`)) && bytes.Contains(body, []byte(`"result"`))
	hasStoredItemID := stripStoredItemIDs && bytes.Contains(body, []byte(`"id"`))
	if !hasDataURL && !hasIGResult && !hasStoredItemID {
		return body
	}
	seq := 0
	var lastImage *codexHistoryImage
	remember := func(dataURL string) {
		if img, ok := parseCodexDataURLImage(dataURL); ok {
			// Keep only the latest image from this request (bounded: 1).
			cp := img
			lastImage = &cp
		}
	}
	nextPlaceholder := func(alt string) string {
		seq++
		alt = strings.TrimSpace(alt)
		if alt == "" {
			alt = "generated image"
		}
		// Keep alt for model semantics; URL is a stable non-pixel ref.
		return fmt.Sprintf("![%s](cliproxy-image:%d)", alt, seq)
	}

	// Single GetBytes("input") — gjson copies the matched raw, so never call it twice on multi-MB bodies.
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		if next, ok := replaceCodexDataURLImagesInText(input.String(), nextPlaceholder, remember); ok {
			return util.MutateTopLevelObject(body, map[string][]byte{
				"input": util.JSONString(next),
			}, nil)
		}
		// Cannot attach input_image to string input safely; placeholders only.
		return body
	}
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	// Keep original raw strings for unchanged fragments; only allocate rewritten ones.
	itemRaws := make([]string, 0, len(items))
	changed := false
	outLen := 2 // []
	appendItem := func(raw string) {
		if len(itemRaws) > 0 {
			outLen++
		}
		itemRaws = append(itemRaws, raw)
		outLen += len(raw)
	}
	for _, item := range items {
		itemRaw := item.Raw
		// Drop base64 from any re-sent image_generation_call history items; remember last.
		if strings.TrimSpace(item.Get("type").String()) == "image_generation_call" {
			if result := item.Get("result"); result.Exists() {
				resultStr := strings.TrimSpace(result.String())
				if len(resultStr) >= 256 {
					// Only reattach if within soft cap; avoid building a huge data URL otherwise.
					if len(resultStr) <= 8<<20 {
						mime := codexImageMIMEFromFormat(item.Get("output_format").String())
						remember(fmt.Sprintf("data:%s;base64,%s", mime, resultStr))
					}
					if next, err := sjson.Delete(item.Raw, "result"); err == nil {
						itemRaw = next
						changed = true
					}
				}
			}
		} else if hasDataURL {
			// message / content text fields
			if content := item.Get("content"); content.Type == gjson.String {
				if next, ok := replaceCodexDataURLImagesInText(content.String(), nextPlaceholder, remember); ok {
					if rewritten, err := sjson.Set(itemRaw, "content", next); err == nil {
						itemRaw = rewritten
						changed = true
					}
				}
			} else if content.IsArray() {
				parts := content.Array()
				partRaws := make([]string, len(parts))
				partChanged := false
				partOutLen := 2
				for partIndex, part := range parts {
					partRaws[partIndex] = part.Raw
					partType := strings.TrimSpace(part.Get("type").String())
					// Only rewrite text parts. Do not touch structured input_image (user uploads /
					// vision) — those are intentional pixels, not Desktop session replay of our
					// outbound markdown data URLs.
					if partType == "input_text" || partType == "output_text" || partType == "text" {
						text := part.Get("text").String()
						if next, ok := replaceCodexDataURLImagesInText(text, nextPlaceholder, remember); ok {
							if rewritten, err := sjson.Set(part.Raw, "text", next); err == nil {
								partRaws[partIndex] = rewritten
								partChanged = true
							}
						}
					}
					if partIndex > 0 {
						partOutLen++
					}
					partOutLen += len(partRaws[partIndex])
				}
				if partChanged {
					var contentBuf bytes.Buffer
					contentBuf.Grow(partOutLen)
					contentBuf.WriteByte('[')
					for i, p := range partRaws {
						if i > 0 {
							contentBuf.WriteByte(',')
						}
						contentBuf.WriteString(p)
					}
					contentBuf.WriteByte(']')
					if rewritten, err := sjson.SetRaw(itemRaw, "content", contentBuf.String()); err == nil {
						itemRaw = rewritten
						changed = true
					}
				}
			}
		}
		if stripStoredItemIDs {
			var itemChanged, drop bool
			itemRaw, itemChanged, drop = stripCodexStoredHistoryItemReference(itemRaw)
			changed = changed || itemChanged
			if drop {
				continue
			}
		}
		appendItem(itemRaw)
	}

	// Re-identify / edit: move last history image into structured input_image (not text tokens).
	if lastImage != nil {
		if nextItems, ok := attachCodexHistoryImageToItemRaws(itemRaws, *lastImage); ok {
			itemRaws = nextItems
			changed = true
			outLen = 2
			for i, raw := range itemRaws {
				if i > 0 {
					outLen++
				}
				outLen += len(raw)
			}
		}
	}
	if !changed {
		return body
	}

	var inputBuf bytes.Buffer
	inputBuf.Grow(outLen)
	inputBuf.WriteByte('[')
	for i, raw := range itemRaws {
		if i > 0 {
			inputBuf.WriteByte(',')
		}
		inputBuf.WriteString(raw)
	}
	inputBuf.WriteByte(']')
	return util.MutateTopLevelObject(body, map[string][]byte{
		"input": inputBuf.Bytes(),
	}, nil)
}

func replaceCodexDataURLImagesInText(text string, nextPlaceholder func(alt string) string, remember func(dataURL string)) (string, bool) {
	if text == "" || !strings.Contains(text, "data:image/") {
		return text, false
	}
	out := text
	out = codexMarkdownDataURLImage.ReplaceAllStringFunc(out, func(match string) string {
		sub := codexMarkdownDataURLImage.FindStringSubmatch(match)
		alt := ""
		if len(sub) >= 2 {
			alt = sub[1]
		}
		if len(sub) >= 3 && remember != nil {
			remember(sub[2])
		}
		return nextPlaceholder(alt)
	})
	// Remaining bare data URLs (not wrapped in markdown).
	if strings.Contains(out, "data:image/") {
		out = codexBareDataURLImage.ReplaceAllStringFunc(out, func(match string) string {
			if remember != nil {
				remember(match)
			}
			return nextPlaceholder("generated image")
		})
	}
	if out == text {
		return text, false
	}
	return out, true
}

func parseCodexDataURLImage(dataURL string) (codexHistoryImage, bool) {
	dataURL = strings.TrimSpace(dataURL)
	if !strings.HasPrefix(dataURL, "data:image/") {
		return codexHistoryImage{}, false
	}
	// data:image/png;base64,<payload>
	rest := strings.TrimPrefix(dataURL, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return codexHistoryImage{}, false
	}
	meta, payload := parts[0], strings.TrimSpace(parts[1])
	if !strings.Contains(meta, "base64") || len(payload) < 256 {
		return codexHistoryImage{}, false
	}
	// Soft cap: refuse absurd single images to protect request path (no store, but still RAM).
	// 8MB base64 ≈ ~6MB binary — enough for normal Codex outputs, blocks pathological payloads.
	if len(payload) > 8<<20 {
		return codexHistoryImage{}, false
	}
	mime := strings.TrimSpace(strings.Split(meta, ";")[0])
	if mime == "" {
		mime = "image/png"
	}
	return codexHistoryImage{MIME: mime, Result: payload}, true
}

// attachCodexHistoryImageToItemRaws adds at most one input_image to the latest user message
// inside a pre-rewritten input item list. Mutates only that item fragment.
func attachCodexHistoryImageToItemRaws(items []string, img codexHistoryImage) ([]string, bool) {
	lastUser := -1
	for i := len(items) - 1; i >= 0; i-- {
		if strings.TrimSpace(gjson.Get(items[i], "role").String()) == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return items, false
	}
	itemRaw := items[lastUser]
	item := gjson.Parse(itemRaw)
	// Skip if this user turn already has an input_image (user-uploaded).
	if content := item.Get("content"); content.IsArray() {
		for _, part := range content.Array() {
			if strings.TrimSpace(part.Get("type").String()) == "input_image" {
				return items, false
			}
		}
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", img.MIME, img.Result)
	imagePartJSON, err := sjson.Set(`{"type":"input_image"}`, "image_url", dataURL)
	if err != nil {
		return items, false
	}

	content := item.Get("content")
	var rewritten string
	switch {
	case content.Type == gjson.String:
		// Promote string content to multipart so we can attach the image.
		textPart, err := sjson.Set(`{"type":"input_text"}`, "text", content.String())
		if err != nil {
			return items, false
		}
		var contentBuf bytes.Buffer
		contentBuf.WriteByte('[')
		contentBuf.WriteString(textPart)
		contentBuf.WriteByte(',')
		contentBuf.WriteString(imagePartJSON)
		contentBuf.WriteByte(']')
		rewritten, err = sjson.SetRaw(itemRaw, "content", contentBuf.String())
		if err != nil {
			return items, false
		}
	case content.IsArray():
		rewritten, err = sjson.SetRaw(itemRaw, "content.-1", imagePartJSON)
		if err != nil {
			return items, false
		}
	default:
		var contentBuf bytes.Buffer
		contentBuf.WriteByte('[')
		contentBuf.WriteString(imagePartJSON)
		contentBuf.WriteByte(']')
		rewritten, err = sjson.SetRaw(itemRaw, "content", contentBuf.String())
		if err != nil {
			return items, false
		}
	}
	out := make([]string, len(items))
	copy(out, items)
	out[lastUser] = rewritten
	return out, true
}

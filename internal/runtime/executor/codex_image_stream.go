package executor

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	// Codex Desktop / ChatGPT-style sandbox image refs written into assistant markdown.
	// Only rewrite these when the same response already produced a hosted image_generation_call.
	codexMntDataMarkdownRef = regexp.MustCompile(`!\[([^\]]*)\]\((?:sandbox:)?(/mnt/data/(\d+)\.([A-Za-z0-9]+))\)`)
	codexMntDataBareRef     = regexp.MustCompile(`(?:sandbox:)?/mnt/data/(\d+)\.([A-Za-z0-9]+)`)

	// Any markdown image whose target is not a browser-renderable URL (data:/http:/https:).
	// Models often invent ~/.codex/generated_images/... paths under custom providers.
	codexMarkdownAnyImageRef = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
)

// codexHostedImage is one completed hosted image_generation_call payload for Desktop rewrite.
type codexHostedImage struct {
	Result string
	MIME   string
}

// codexImageStreamNormalizer rewrites Desktop-visible assistant markdown after hosted image gen.
//
// Codex Desktop keeps the model text message (often `![x](/mnt/data/0.png)`) and drops the
// synthetic data-url message we inject beside image_generation_call. When this response
// already has image result(s), replace sandbox /mnt/data refs with data URLs in the
// assistant text events Desktop actually persists.
type codexImageStreamNormalizer struct {
	images []codexHostedImage
}

func newCodexImageStreamNormalizer() *codexImageStreamNormalizer {
	return &codexImageStreamNormalizer{}
}

// normalizeCodexImageGenerationOutboundEvent normalizes hosted image events for clients:
//  1. force status=completed when result is present
//  2. cache image results and rewrite Desktop assistant markdown /mnt/data refs to data URLs
//  3. synthesize a fallback data-url message (Desktop may drop it; rewrite is the primary path)
//
// n may be nil; then only status + synthetic display message run (stateless).
func normalizeCodexImageGenerationOutboundEvent(payload []byte) [][]byte {
	return normalizeCodexImageGenerationOutboundEventWithState(nil, payload)
}

func normalizeCodexImageGenerationOutboundEventWithState(n *codexImageStreamNormalizer, payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	normalized := normalizeCodexImageGenerationCallStatus(payload)
	if n != nil {
		n.observe(normalized)
		normalized = n.rewriteAssistantMarkdown(normalized)
	}
	out := [][]byte{normalized}
	if msg := synthesizeCodexImageDisplayMessageEvent(normalized); len(msg) > 0 {
		out = append(out, msg)
	}
	return out
}

func (n *codexImageStreamNormalizer) observe(payload []byte) {
	if n == nil || len(payload) == 0 {
		return
	}
	body := sseJSONBody(payload)
	if !gjson.ValidBytes(body) {
		return
	}
	switch gjson.GetBytes(body, "type").String() {
	case "response.output_item.done", "response.output_item.added":
		n.rememberImageItem(gjson.GetBytes(body, "item"))
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return
		}
		for _, item := range output.Array() {
			n.rememberImageItem(item)
		}
	}
}

func (n *codexImageStreamNormalizer) rememberImageItem(item gjson.Result) {
	if !item.Exists() || !item.IsObject() {
		return
	}
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return
	}
	// Avoid duplicate cache if both added/done carry the same result.
	for _, existing := range n.images {
		if existing.Result == result {
			return
		}
	}
	n.images = append(n.images, codexHostedImage{
		Result: result,
		MIME:   codexImageMIMEFromFormat(item.Get("output_format").String()),
	})
}

func (n *codexImageStreamNormalizer) rewriteAssistantMarkdown(payload []byte) []byte {
	if n == nil || len(n.images) == 0 || len(payload) == 0 {
		return payload
	}
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := sseJSONBody(payload)
	if !gjson.ValidBytes(body) {
		return payload
	}

	eventType := gjson.GetBytes(body, "type").String()
	updated := body
	changed := false

	switch eventType {
	case "response.output_text.done":
		text := gjson.GetBytes(body, "text").String()
		if next, ok := ensureCodexAssistantImageMarkdown(text, n.images); ok {
			var err error
			updated, err = sjson.SetBytes(updated, "text", next)
			if err != nil {
				return payload
			}
			changed = true
		}
	case "response.content_part.done":
		partType := strings.TrimSpace(gjson.GetBytes(body, "part.type").String())
		if partType == "output_text" || partType == "text" {
			text := gjson.GetBytes(body, "part.text").String()
			if next, ok := ensureCodexAssistantImageMarkdown(text, n.images); ok {
				var err error
				updated, err = sjson.SetBytes(updated, "part.text", next)
				if err != nil {
					return payload
				}
				changed = true
			}
		}
	case "response.output_item.done", "response.output_item.added":
		item := gjson.GetBytes(body, "item")
		if strings.TrimSpace(item.Get("type").String()) != "message" {
			return payload
		}
		// Skip our synthetic display item; Desktop ignores it and we must not recurse on it.
		if isCodexSyntheticImageDisplayMessageID(item.Get("id").String()) {
			return payload
		}
		// Only rewrite completed assistant messages Desktop persists as agent text.
		role := strings.TrimSpace(item.Get("role").String())
		if role != "" && role != "assistant" {
			return payload
		}
		// output_item.added is often empty; only rewrite/append on done so deltas stay coherent.
		appendOK := eventType == "response.output_item.done"
		content := item.Get("content")
		if !content.IsArray() {
			return payload
		}
		for i, part := range content.Array() {
			partType := strings.TrimSpace(part.Get("type").String())
			if partType != "output_text" && partType != "text" {
				continue
			}
			text := part.Get("text").String()
			var next string
			var ok bool
			if appendOK {
				next, ok = ensureCodexAssistantImageMarkdown(text, n.images)
			} else {
				next, ok = rewriteCodexMntDataImageMarkdown(text, n.images)
			}
			if !ok {
				continue
			}
			path := "item.content." + strconv.Itoa(i) + ".text"
			var err error
			updated, err = sjson.SetBytes(updated, path, next)
			if err != nil {
				return payload
			}
			changed = true
		}
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return payload
		}
		for i, item := range output.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "message" {
				continue
			}
			if isCodexSyntheticImageDisplayMessageID(item.Get("id").String()) {
				continue
			}
			role := strings.TrimSpace(item.Get("role").String())
			if role != "" && role != "assistant" {
				continue
			}
			content := item.Get("content")
			if !content.IsArray() {
				continue
			}
			for j, part := range content.Array() {
				partType := strings.TrimSpace(part.Get("type").String())
				if partType != "output_text" && partType != "text" {
					continue
				}
				text := part.Get("text").String()
				next, ok := ensureCodexAssistantImageMarkdown(text, n.images)
				if !ok {
					continue
				}
				path := "response.output." + strconv.Itoa(i) + ".content." + strconv.Itoa(j) + ".text"
				var err error
				updated, err = sjson.SetBytes(updated, path, next)
				if err != nil {
					return payload
				}
				changed = true
			}
		}
	default:
		return payload
	}
	if !changed {
		return payload
	}
	return maybeWrapSSEData(hadSSEPrefix, updated)
}

// ensureCodexAssistantImageMarkdown makes Desktop-visible assistant text show hosted images.
//  1. Rewrite ChatGPT sandbox refs: ![x](/mnt/data/0.png) -> data URL
//  2. Rewrite non-renderable local paths: ![x](/Users/.../generated_images/a.png) -> data URL
//  3. Only if still no data:image, append one markdown data URL (never stack on top of a fake path).
func ensureCodexAssistantImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if len(images) == 0 {
		return text, false
	}
	changed := false
	if next, ok := rewriteCodexMntDataImageMarkdown(text, images); ok {
		text = next
		changed = true
	}
	if next, ok := rewriteCodexNonRenderableImageMarkdown(text, images); ok {
		text = next
		changed = true
	}
	// Already has a renderable data image — do not append a second copy (double-thumbnail bug).
	if strings.Contains(text, "data:image/") {
		return text, changed
	}
	return appendCodexHostedImageMarkdown(text, images), true
}

func rewriteCodexMntDataImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if text == "" || len(images) == 0 || !strings.Contains(text, "/mnt/data/") {
		return text, false
	}
	// Prefer markdown image refs so alt text is preserved.
	out := codexMntDataMarkdownRef.ReplaceAllStringFunc(text, func(match string) string {
		sub := codexMntDataMarkdownRef.FindStringSubmatch(match)
		if len(sub) != 5 {
			return match
		}
		img, ok := codexHostedImageByIndex(images, sub[3])
		if !ok {
			return match
		}
		return fmt.Sprintf("![%s](data:%s;base64,%s)", sub[1], img.MIME, img.Result)
	})
	// Bare /mnt/data/N.ext left outside markdown (rare).
	if strings.Contains(out, "/mnt/data/") {
		out = codexMntDataBareRef.ReplaceAllStringFunc(out, func(match string) string {
			sub := codexMntDataBareRef.FindStringSubmatch(match)
			if len(sub) != 3 {
				return match
			}
			img, ok := codexHostedImageByIndex(images, sub[1])
			if !ok {
				return match
			}
			return fmt.Sprintf("data:%s;base64,%s", img.MIME, img.Result)
		})
	}
	if out == text {
		return text, false
	}
	return out, true
}

func appendCodexHostedImageMarkdown(text string, images []codexHostedImage) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, " \t\r\n"))
	for i, img := range images {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		alt := "generated image"
		if len(images) > 1 {
			alt = fmt.Sprintf("generated image %d", i+1)
		}
		b.WriteString("![")
		b.WriteString(alt)
		b.WriteString("](data:")
		b.WriteString(img.MIME)
		b.WriteString(";base64,")
		b.WriteString(img.Result)
		b.WriteByte(')')
	}
	return b.String()
}

// rewriteCodexNonRenderableImageMarkdown replaces ![alt](local-or-fake-path) with data URLs.
// Leaves data:/http(s):/cliproxy-image: targets alone.
func rewriteCodexNonRenderableImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if text == "" || len(images) == 0 || !strings.Contains(text, "![") {
		return text, false
	}
	seq := 0
	out := codexMarkdownAnyImageRef.ReplaceAllStringFunc(text, func(match string) string {
		sub := codexMarkdownAnyImageRef.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		alt, target := sub[1], strings.TrimSpace(sub[2])
		if target == "" || isCodexRenderableImageTarget(target) {
			return match
		}
		img := images[0]
		if seq < len(images) {
			img = images[seq]
		} else {
			img = images[len(images)-1]
		}
		seq++
		return fmt.Sprintf("![%s](data:%s;base64,%s)", alt, img.MIME, img.Result)
	})
	if out == text {
		return text, false
	}
	return out, true
}

func isCodexRenderableImageTarget(target string) bool {
	lower := strings.ToLower(strings.TrimSpace(target))
	switch {
	case strings.HasPrefix(lower, "data:image/"):
		return true
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return true
	case strings.HasPrefix(lower, "cliproxy-image:"):
		return true
	default:
		return false
	}
}

func isCodexSyntheticImageDisplayMessageID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	return strings.HasSuffix(id, "_display") || strings.HasPrefix(id, "msg_ig_")
}

func codexHostedImageByIndex(images []codexHostedImage, idxStr string) (codexHostedImage, bool) {
	if len(images) == 0 {
		return codexHostedImage{}, false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return codexHostedImage{}, false
	}
	if idx >= len(images) {
		// Single-image turns often only emit 0.png; clamp overshoot to last result.
		idx = len(images) - 1
	}
	return images[idx], true
}

func codexImageMIMEFromFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

func sseJSONBody(payload []byte) []byte {
	if bytes.HasPrefix(payload, dataTag) {
		return bytes.TrimSpace(payload[len(dataTag):])
	}
	return payload
}

// normalizeCodexImageGenerationCallStatus upgrades image_generation_call items that already
// carry a finished image payload but remain status=generating.
func normalizeCodexImageGenerationCallStatus(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := payload
	if hadSSEPrefix {
		body = bytes.TrimSpace(payload[len(dataTag):])
	}
	if !gjson.ValidBytes(body) {
		return payload
	}

	eventType := gjson.GetBytes(body, "type").String()
	switch eventType {
	case "response.output_item.done", "response.output_item.added":
		if !shouldCompleteImageGenerationCall(gjson.GetBytes(body, "item")) {
			return payload
		}
		updated, err := sjson.SetBytes(body, "item.status", "completed")
		if err != nil {
			return payload
		}
		return maybeWrapSSEData(hadSSEPrefix, updated)
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return payload
		}
		updated := body
		changed := false
		for index, item := range output.Array() {
			if !shouldCompleteImageGenerationCall(item) {
				continue
			}
			path := "response.output." + strconv.Itoa(index) + ".status"
			next, err := sjson.SetBytes(updated, path, "completed")
			if err != nil {
				return payload
			}
			updated = next
			changed = true
		}
		if !changed {
			return payload
		}
		return maybeWrapSSEData(hadSSEPrefix, updated)
	default:
		return payload
	}
}

// synthesizeCodexImageDisplayMessageEvent turns a completed hosted image_generation_call
// into an assistant markdown image message. Desktop custom/API-key providers often cannot
// expose local image_gen, so hosted base64 results otherwise stay invisible.
func synthesizeCodexImageDisplayMessageEvent(payload []byte) []byte {
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := payload
	if hadSSEPrefix {
		body = bytes.TrimSpace(payload[len(dataTag):])
	}
	if !gjson.ValidBytes(body) {
		return nil
	}
	if gjson.GetBytes(body, "type").String() != "response.output_item.done" {
		return nil
	}
	item := gjson.GetBytes(body, "item")
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return nil
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(item.Get("status").String()))
	if status != "" && status != "completed" && status != "generating" && status != "in_progress" && status != "incomplete" {
		return nil
	}
	mime := codexImageMIMEFromFormat(item.Get("output_format").String())
	// Markdown image so Desktop/agent markdown renderers can show the asset inline.
	// Fallback only: Desktop often drops this synthetic item; stream rewrite is primary.
	text := fmt.Sprintf("![generated image](data:%s;base64,%s)", mime, result)
	msgID := strings.TrimSpace(item.Get("id").String())
	if msgID == "" {
		msgID = "msg_image_display"
	} else {
		msgID = "msg_" + msgID + "_display"
	}
	event := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":""}]}}`)
	event, _ = sjson.SetBytes(event, "item.id", msgID)
	event, _ = sjson.SetBytes(event, "item.content.0.text", text)
	if revised := strings.TrimSpace(item.Get("revised_prompt").String()); revised != "" {
		// Keep caption short; full prompt can be huge.
		if len(revised) > 200 {
			revised = revised[:200] + "…"
		}
		caption := "Generated image: " + revised + "\n\n" + text
		event, _ = sjson.SetBytes(event, "item.content.0.text", caption)
	}
	return maybeWrapSSEData(hadSSEPrefix, event)
}

func shouldCompleteImageGenerationCall(item gjson.Result) bool {
	if !item.Exists() || !item.IsObject() {
		return false
	}
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("status").String())) {
	case "", "generating", "in_progress", "incomplete":
		return true
	default:
		return false
	}
}

func maybeWrapSSEData(hadSSEPrefix bool, body []byte) []byte {
	if !hadSSEPrefix {
		return body
	}
	out := make([]byte, 0, len(dataTag)+1+len(body))
	out = append(out, dataTag...)
	out = append(out, ' ')
	out = append(out, body...)
	return out
}

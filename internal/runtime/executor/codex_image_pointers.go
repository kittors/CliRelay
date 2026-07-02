package executor

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func readCodexImageConversationStream(r io.Reader) (string, []codexImagePointer, error) {
	reader := bufio.NewReader(r)
	var conversationID string
	var pointers []codexImagePointer
	for {
		line, err := reader.ReadString('\n')
		if data, ok := codexExtractSSEDataLine(strings.TrimRight(line, "\r\n")); ok && data != "" && data != "[DONE]" {
			dataBytes := []byte(data)
			if conversationID == "" {
				conversationID = strings.TrimSpace(gjson.GetBytes(dataBytes, "v.conversation_id").String())
				if conversationID == "" {
					conversationID = strings.TrimSpace(gjson.GetBytes(dataBytes, "conversation_id").String())
				}
			}
			pointers = mergeCodexImagePointers(pointers, collectCodexImagePointers(dataBytes))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, err
		}
	}
	return conversationID, pointers, nil
}

func codexExtractSSEDataLine(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")), true
}

func collectCodexImagePointers(body []byte) []codexImagePointer {
	if len(body) == 0 {
		return nil
	}
	matches := codexImagePointerMatches(body)
	prompt := ""
	for _, path := range []string{"message.metadata.dalle.prompt", "metadata.dalle.prompt", "revised_prompt"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			prompt = value
			break
		}
	}
	out := make([]codexImagePointer, 0, len(matches))
	for _, pointer := range matches {
		out = append(out, codexImagePointer{Pointer: pointer, Prompt: prompt})
	}
	return mergeCodexImagePointers(out, collectCodexImageInlineAssets(body, prompt))
}

func codexImagePointerMatches(body []byte) []string {
	raw := string(body)
	matches := make([]string, 0, 4)
	for _, prefix := range []string{"file-service://", "sediment://"} {
		start := 0
		for {
			idx := strings.Index(raw[start:], prefix)
			if idx < 0 {
				break
			}
			idx += start
			end := idx + len(prefix)
			for end < len(raw) {
				ch := raw[end]
				if ch != '-' && ch != '_' && (ch < '0' || ch > '9') && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') {
					break
				}
				end++
			}
			matches = append(matches, raw[idx:end])
			start = end
		}
	}
	return dedupeCodexImageStrings(matches)
}

func collectCodexImageInlineAssets(body []byte, fallbackPrompt string) []codexImagePointer {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil
	}
	var out []codexImagePointer
	walkCodexImageInlineAssets(decoded, strings.TrimSpace(fallbackPrompt), &out)
	return out
}

func walkCodexImageInlineAssets(node any, prompt string, out *[]codexImagePointer) {
	switch value := node.(type) {
	case map[string]any:
		localPrompt := prompt
		for _, key := range []string{"revised_prompt", "image_gen_title", "prompt"} {
			if v, ok := value[key].(string); ok && strings.TrimSpace(v) != "" {
				localPrompt = strings.TrimSpace(v)
				break
			}
		}
		item := codexImagePointer{
			Prompt:      localPrompt,
			Pointer:     firstCodexImageNonEmptyString(value["asset_pointer"], value["pointer"]),
			DownloadURL: firstCodexImageNonEmptyString(value["download_url"], value["url"], value["image_url"]),
			B64JSON:     firstCodexImageNonEmptyString(value["b64_json"], value["base64"], value["image_base64"]),
			MimeType:    firstCodexImageNonEmptyString(value["mime_type"], value["mimeType"], value["content_type"]),
		}
		switch {
		case strings.HasPrefix(strings.TrimSpace(item.Pointer), "file-service://"),
			strings.HasPrefix(strings.TrimSpace(item.Pointer), "sediment://"),
			isLikelyCodexImageDownloadURL(item.DownloadURL),
			normalizeCodexImageBase64(item.B64JSON) != "":
			*out = append(*out, item)
		}
		for _, child := range value {
			walkCodexImageInlineAssets(child, localPrompt, out)
		}
	case []any:
		for _, child := range value {
			walkCodexImageInlineAssets(child, prompt, out)
		}
	}
}

func firstCodexImageNonEmptyString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func mergeCodexImagePointers(existing []codexImagePointer, next []codexImagePointer) []codexImagePointer {
	if len(next) == 0 {
		return existing
	}
	seen := make(map[string]codexImagePointer, len(existing)+len(next))
	out := make([]codexImagePointer, 0, len(existing)+len(next))
	for _, item := range existing {
		if key := item.identityKey(); key != "" {
			seen[key] = item
		}
		out = append(out, item)
	}
	for _, item := range next {
		key := item.identityKey()
		if key == "" {
			continue
		}
		if existingItem, ok := seen[key]; ok {
			merged := mergeCodexImagePointer(existingItem, item)
			if merged != existingItem {
				for i := range out {
					if out[i].identityKey() == key {
						out[i] = merged
						break
					}
				}
				seen[key] = merged
			}
			continue
		}
		seen[key] = item
		out = append(out, item)
	}
	return out
}

func (p codexImagePointer) identityKey() string {
	switch {
	case strings.TrimSpace(p.Pointer) != "":
		return "pointer:" + strings.TrimSpace(p.Pointer)
	case strings.TrimSpace(p.DownloadURL) != "":
		return "download:" + strings.TrimSpace(p.DownloadURL)
	case strings.TrimSpace(p.B64JSON) != "":
		b64 := strings.TrimSpace(p.B64JSON)
		if len(b64) > 64 {
			b64 = b64[:64]
		}
		return "b64:" + b64
	default:
		return ""
	}
}

func mergeCodexImagePointer(existing, next codexImagePointer) codexImagePointer {
	merged := existing
	if strings.TrimSpace(merged.Pointer) == "" {
		merged.Pointer = next.Pointer
	}
	if strings.TrimSpace(merged.DownloadURL) == "" {
		merged.DownloadURL = next.DownloadURL
	}
	if strings.TrimSpace(merged.B64JSON) == "" {
		merged.B64JSON = next.B64JSON
	}
	if strings.TrimSpace(merged.MimeType) == "" {
		merged.MimeType = next.MimeType
	}
	if strings.TrimSpace(merged.Prompt) == "" {
		merged.Prompt = next.Prompt
	}
	return merged
}

func hasCodexFileServicePointer(items []codexImagePointer) bool {
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") {
			return true
		}
	}
	return false
}

func preferCodexFileServicePointers(items []codexImagePointer) []codexImagePointer {
	if !hasCodexFileServicePointer(items) {
		return items
	}
	out := make([]codexImagePointer, 0, len(items))
	for _, item := range items {
		if strings.HasPrefix(item.Pointer, "file-service://") {
			out = append(out, item)
		}
	}
	return out
}

func extractCodexImageToolMessages(mapping map[string]any) []codexImageToolMessage {
	if len(mapping) == 0 {
		return nil
	}
	out := make([]codexImageToolMessage, 0, 4)
	for _, raw := range mapping {
		node, _ := raw.(map[string]any)
		if node == nil {
			continue
		}
		message, _ := node["message"].(map[string]any)
		if message == nil {
			continue
		}
		author, _ := message["author"].(map[string]any)
		metadata, _ := message["metadata"].(map[string]any)
		content, _ := message["content"].(map[string]any)
		if author == nil || metadata == nil || content == nil {
			continue
		}
		if role, _ := author["role"].(string); role != "tool" {
			continue
		}
		if asyncTaskType, _ := metadata["async_task_type"].(string); asyncTaskType != "image_gen" {
			continue
		}
		if contentType, _ := content["content_type"].(string); contentType != "multimodal_text" {
			continue
		}
		prompt := ""
		if title, _ := metadata["image_gen_title"].(string); strings.TrimSpace(title) != "" {
			prompt = strings.TrimSpace(title)
		}
		item := codexImageToolMessage{}
		if createTime, ok := message["create_time"].(float64); ok {
			item.CreateTime = createTime
		}
		parts, _ := content["parts"].([]any)
		for _, part := range parts {
			switch value := part.(type) {
			case map[string]any:
				pointer := codexImagePointer{
					Prompt:      prompt,
					Pointer:     firstCodexImageNonEmptyString(value["asset_pointer"], value["pointer"]),
					DownloadURL: firstCodexImageNonEmptyString(value["download_url"], value["url"], value["image_url"]),
					B64JSON:     firstCodexImageNonEmptyString(value["b64_json"], value["base64"], value["image_base64"]),
					MimeType:    firstCodexImageNonEmptyString(value["mime_type"], value["mimeType"], value["content_type"]),
				}
				if pointer.identityKey() != "" {
					item.Pointers = append(item.Pointers, pointer)
				}
			case string:
				for _, match := range codexImagePointerMatches([]byte(value)) {
					item.Pointers = append(item.Pointers, codexImagePointer{Pointer: match, Prompt: prompt})
				}
			}
		}
		item.Pointers = mergeCodexImagePointers(nil, item.Pointers)
		if len(item.Pointers) == 0 {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreateTime < out[j].CreateTime
	})
	return out
}

func collectCodexImagePollPointers(body []byte) []codexImagePointer {
	pointers := mergeCodexImagePointers(nil, collectCodexImagePointers(body))
	if len(body) == 0 {
		return pointers
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err == nil {
		if mapping, _ := decoded["mapping"].(map[string]any); len(mapping) > 0 {
			toolMessages := extractCodexImageToolMessages(mapping)
			toolPointers := make([]codexImagePointer, 0, len(toolMessages))
			for _, msg := range toolMessages {
				toolPointers = mergeCodexImagePointers(toolPointers, msg.Pointers)
			}
			pointers = mergeCodexImagePointers(pointers, toolPointers)
		}
	}
	return preferCodexFileServicePointers(pointers)
}

func pollCodexImageConversation(ctx context.Context, client *http.Client, headers http.Header, conversationID string) ([]codexImagePointer, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return nil, nil
	}
	startedAt := time.Now()
	deadline := startedAt.Add(codexImagePollTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	var lastErr error
	for {
		if timeoutErr := codexImagePollTimeoutError(ctx, startedAt, deadline, conversationID); timeoutErr != nil {
			if lastErr != nil && !errors.Is(lastErr, context.Canceled) && !errors.Is(lastErr, context.DeadlineExceeded) {
				return nil, lastErr
			}
			return nil, timeoutErr
		}
		resp, err := doCodexImageJSON(ctx, client, http.MethodGet, codexImageURL("/backend-api/conversation/"+conversationID), headers, nil) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
		if err != nil {
			if timeoutErr := codexImagePollTimeoutError(ctx, startedAt, deadline, conversationID); timeoutErr != nil {
				return nil, timeoutErr
			}
			lastErr = err
		} else {
			body, readErr := readAndCloseCodexImageBody(resp)
			if readErr != nil {
				lastErr = readErr
			} else if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				return nil, codexImageStatusErrWithBody(resp.StatusCode, body, "conversation poll failed")
			} else {
				var decoded map[string]any
				toolMessages := 0
				toolPointers := 0
				genericPointers := len(collectCodexImagePointers(body))
				if err := json.Unmarshal(body, &decoded); err == nil {
					if mapping, _ := decoded["mapping"].(map[string]any); len(mapping) > 0 {
						messages := extractCodexImageToolMessages(mapping)
						toolMessages = len(messages)
						for _, msg := range messages {
							toolPointers += len(msg.Pointers)
						}
					}
				}
				pointers := collectCodexImagePollPointers(body)
				log.Debugf(
					"codex image poll conversation=%s tool_messages=%d tool_assets=%d generic_assets=%d filtered_assets=%d",
					conversationID,
					toolMessages,
					toolPointers,
					genericPointers,
					len(pointers),
				)
				if len(pointers) > 0 {
					return pointers, nil
				}
				if textReply := extractCompletedCodexImageAssistantText(body); strings.TrimSpace(textReply) != "" {
					return nil, statusErr{
						code: http.StatusBadGateway,
						msg: fmt.Sprintf(
							"openai image conversation completed without image assets (conversation_id=%s, assistant_text=%q)",
							conversationID,
							textReply,
						),
					}
				}
			}
		}
		if timeoutErr := codexImagePollTimeoutError(ctx, startedAt, deadline, conversationID); timeoutErr != nil {
			if lastErr != nil && !errors.Is(lastErr, context.Canceled) && !errors.Is(lastErr, context.DeadlineExceeded) {
				return nil, lastErr
			}
			return nil, timeoutErr
		}
		timer := time.NewTimer(codexImagePollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if timeoutErr := codexImagePollTimeoutError(ctx, startedAt, deadline, conversationID); timeoutErr != nil {
				return nil, timeoutErr
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func extractCompletedCodexImageAssistantText(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	currentNodeID := strings.TrimSpace(gjson.GetBytes(body, "current_node").String())
	if currentNodeID != "" {
		if text := completedCodexImageAssistantTextAtPath(body, "mapping."+currentNodeID+".message"); text != "" {
			return text
		}
	}
	mapping := gjson.GetBytes(body, "mapping")
	if !mapping.IsObject() {
		return ""
	}
	result := ""
	mapping.ForEach(func(_, node gjson.Result) bool {
		if text := completedCodexImageAssistantTextAtPathBytes([]byte(node.Raw), "message"); text != "" {
			result = text
			return false
		}
		return true
	})
	return result
}

func completedCodexImageAssistantTextAtPath(body []byte, path string) string {
	return completedCodexImageAssistantTextAtPathBytes(body, path)
}

func completedCodexImageAssistantTextAtPathBytes(body []byte, path string) string {
	message := gjson.GetBytes(body, path)
	if !message.Exists() {
		return ""
	}
	if strings.TrimSpace(message.Get("author.role").String()) != "assistant" {
		return ""
	}
	status := strings.TrimSpace(message.Get("status").String())
	if status != "finished_successfully" && status != "finished" {
		return ""
	}
	if strings.TrimSpace(message.Get("content.content_type").String()) != "text" {
		return ""
	}
	parts := message.Get("content.parts")
	if !parts.IsArray() || parts.Array() == nil {
		return ""
	}
	texts := make([]string, 0, 2)
	parts.ForEach(func(_, part gjson.Result) bool {
		if value := strings.TrimSpace(part.String()); value != "" {
			texts = append(texts, value)
		}
		return true
	})
	if len(texts) == 0 {
		return ""
	}
	return strings.Join(texts, "\n")
}

func codexImagePollTimeoutError(ctx context.Context, startedAt, deadline time.Time, conversationID string) error {
	now := time.Now()
	timedOut := !deadline.IsZero() && !now.Before(deadline)
	if !timedOut {
		if ctx == nil || ctx.Err() == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil
		}
		timedOut = true
	}
	if !timedOut {
		return nil
	}
	waited := now.Sub(startedAt)
	if waited <= 0 {
		waited = codexImagePollTimeout
	}
	return statusErr{
		code: http.StatusGatewayTimeout,
		msg: fmt.Sprintf(
			"openai image conversation timed out after %s without any generated image assets (conversation_id=%s)",
			waited.Round(time.Second),
			conversationID,
		),
	}
}

func buildCodexImageOpenAIResponse(
	ctx context.Context,
	client *http.Client,
	headers http.Header,
	conversationID string,
	pointers []codexImagePointer,
) ([]byte, error) {
	type responseItem struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	items := make([]responseItem, 0, len(pointers))
	for _, pointer := range pointers {
		data, err := resolveCodexImageBytes(ctx, client, headers, conversationID, pointer)
		if err != nil {
			return nil, err
		}
		items = append(items, responseItem{
			B64JSON:       base64.StdEncoding.EncodeToString(data),
			RevisedPrompt: pointer.Prompt,
		})
	}
	return json.Marshal(map[string]any{
		"created": time.Now().Unix(),
		"data":    items,
	})
}

func resolveCodexImageBytes(
	ctx context.Context,
	client *http.Client,
	headers http.Header,
	conversationID string,
	pointer codexImagePointer,
) ([]byte, error) {
	if normalized := normalizeCodexImageBase64(pointer.B64JSON); normalized != "" {
		return base64.StdEncoding.DecodeString(normalized)
	}
	if normalized := normalizeCodexImageBase64(pointer.DownloadURL); normalized != "" {
		return base64.StdEncoding.DecodeString(normalized)
	}
	if downloadURL := strings.TrimSpace(pointer.DownloadURL); downloadURL != "" {
		return downloadCodexImageBytes(ctx, client, headers, downloadURL)
	}
	if strings.TrimSpace(pointer.Pointer) == "" {
		return nil, fmt.Errorf("image asset is missing pointer, url, and base64 data")
	}
	downloadURL, err := fetchCodexImageDownloadURL(ctx, client, headers, conversationID, pointer.Pointer)
	if err != nil {
		return nil, err
	}
	return downloadCodexImageBytes(ctx, client, headers, downloadURL)
}

func normalizeCodexImageBase64(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		if idx := strings.Index(raw, ","); idx >= 0 && idx+1 < len(raw) {
			raw = raw[idx+1:]
		}
	}
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "=")
	raw += strings.Repeat("=", (4-len(raw)%4)%4)
	if raw == "" {
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
		return ""
	}
	return raw
}

func isLikelyCodexImageDownloadURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:image/") {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(raw), "http://") && !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return false
	}
	lower := strings.ToLower(raw)
	return strings.Contains(lower, "/download") ||
		strings.Contains(lower, ".png") ||
		strings.Contains(lower, ".jpg") ||
		strings.Contains(lower, ".jpeg") ||
		strings.Contains(lower, ".webp")
}

func fetchCodexImageDownloadURL(ctx context.Context, client *http.Client, headers http.Header, conversationID string, pointer string) (string, error) {
	url := ""
	switch {
	case strings.HasPrefix(pointer, "file-service://"):
		fileID := strings.TrimPrefix(pointer, "file-service://")
		url = codexImageURL("/backend-api/files/" + fileID + "/download")
	case strings.HasPrefix(pointer, "sediment://"):
		attachmentID := strings.TrimPrefix(pointer, "sediment://")
		if strings.TrimSpace(conversationID) == "" {
			return "", fmt.Errorf("conversation id is required for sediment image pointer")
		}
		url = codexImageURL("/backend-api/conversation/" + strings.TrimSpace(conversationID) + "/attachment/" + attachmentID + "/download")
	default:
		return "", fmt.Errorf("unsupported image pointer: %s", pointer)
	}
	resp, err := doCodexImageJSON(ctx, client, http.MethodGet, url, headers, nil) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
	if err != nil {
		return "", err
	}
	body, readErr := readAndCloseCodexImageBody(resp)
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", codexImageStatusErrWithBody(resp.StatusCode, body, "fetch image download url failed")
	}
	var result struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.DownloadURL) == "" {
		return "", fmt.Errorf("fetch image download url returned empty download_url")
	}
	return strings.TrimSpace(result.DownloadURL), nil
}

func downloadCodexImageBytes(ctx context.Context, client *http.Client, headers http.Header, downloadURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", coalesceCodexImageText(headers.Get("User-Agent"), codexImageBackendUserAgent))
	if strings.HasPrefix(downloadURL, codexImageURL("/")) {
		req.Header = cloneHeader(headers)
		req.Header.Set("Accept", "image/*,*/*;q=0.8")
		req.Header.Del("Content-Type")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := readUpstreamResponseBody("codex-image", resp.Body)
		return nil, codexImageStatusErrWithBody(resp.StatusCode, body, "download image bytes failed")
	}
	return readUpstreamResponseBody("codex-image", resp.Body)
}

func codexImageStatusErr(resp *http.Response, fallback string) statusErr {
	if resp == nil {
		return statusErr{code: http.StatusBadGateway, msg: fallback}
	}
	body, _ := readUpstreamResponseBody("codex-image", resp.Body)
	return codexImageStatusErrWithBody(resp.StatusCode, body, fallback)
}

func codexImageStatusErrWithBody(statusCode int, body []byte, fallback string) statusErr {
	message := strings.TrimSpace(extractCodexImageErrorMessage(body))
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = fallback
	}
	return statusErr{code: statusCode, msg: message, upstreamBody: append([]byte(nil), body...)}
}

func extractCodexImageErrorMessage(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{"error.message", "detail", "message"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	return ""
}

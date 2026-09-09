package executor

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// isCodexImageFamilyModel reports whether a model on the /v1/images endpoints is an
// image model this credential pool serves.
//
// Classification comes from the registry rather than a list kept here, so adding a
// model to the static catalog is enough to make it routable. Pinning a single id at
// this gate meant every new gpt-image release was rejected by the proxy before
// upstream ever saw it, which is not a judgement this layer should be making.
func isCodexImageFamilyModel(model string) bool {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return false
	}
	if !registry.IsImageGenerationModel(trimmed) {
		return false
	}
	return strings.EqualFold(registry.ImageGenerationProvider(trimmed), registry.ImageProviderCodex)
}

func parseCodexImageRequest(body []byte) (*codexImageRequest, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("request body is empty")
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("failed to parse request body")
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		model = codexImageModel
	}
	// Admit the whole Codex image family rather than one id. Pinning a single
	// constant here meant every new gpt-image release was rejected by this proxy
	// before upstream ever saw it, which is not a judgement this layer should be
	// making: whether an account can serve a given image model is upstream's answer.
	if !isCodexImageFamilyModel(model) {
		return nil, fmt.Errorf("model %q is not supported by this endpoint", model)
	}
	prompt := strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	parsed := &codexImageRequest{Model: model, Prompt: prompt, N: 1}
	if nResult := gjson.GetBytes(body, "n"); nResult.Exists() {
		if nResult.Type != gjson.Number {
			return nil, fmt.Errorf("n must be a number")
		}
		parsed.N = int(nResult.Int())
	}
	if parsed.N < 1 || parsed.N > codexImageMaxN {
		return nil, fmt.Errorf("n must be between 1 and %d for Codex OAuth image generation", codexImageMaxN)
	}
	if streamResult := gjson.GetBytes(body, "stream"); streamResult.Exists() {
		if streamResult.Type != gjson.True && streamResult.Type != gjson.False {
			return nil, fmt.Errorf("stream must be a boolean")
		}
		parsed.Stream = streamResult.Bool()
	}
	if parsed.Stream {
		return nil, fmt.Errorf("streaming image generation is not supported for Codex OAuth")
	}
	parsed.ResponseFormat = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "response_format").String()))
	if parsed.ResponseFormat != "" && parsed.ResponseFormat != "b64_json" {
		return nil, fmt.Errorf("only response_format=b64_json is supported for Codex OAuth image generation")
	}
	parsed.Size = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "size").String()))
	if parsed.Size != "" && !isValidCodexImageSize(parsed.Size) {
		return nil, fmt.Errorf("size must be WIDTHxHEIGHT using positive integers")
	}
	parsed.Quality = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "quality").String()))
	if parsed.Quality != "" && !isSupportedCodexImageQuality(parsed.Quality) {
		return nil, fmt.Errorf("quality must be one of low, medium, high, xhigh, max, auto")
	}
	parsed.Background = strings.TrimSpace(gjson.GetBytes(body, "background").String())
	parsed.OutputFormat = strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "output_format").String()))
	parsed.Moderation = strings.TrimSpace(gjson.GetBytes(body, "moderation").String())
	parsed.Style = strings.TrimSpace(gjson.GetBytes(body, "style").String())
	parsed.InputFidelity = strings.TrimSpace(gjson.GetBytes(body, "input_fidelity").String())
	if outputCompression := gjson.GetBytes(body, "output_compression"); outputCompression.Exists() {
		if outputCompression.Type != gjson.Number {
			return nil, fmt.Errorf("output_compression must be a number")
		}
		value := int(outputCompression.Int())
		parsed.OutputCompression = &value
	}
	if partialImages := gjson.GetBytes(body, "partial_images"); partialImages.Exists() {
		if partialImages.Type != gjson.Number {
			return nil, fmt.Errorf("partial_images must be a number")
		}
		value := int(partialImages.Int())
		parsed.PartialImages = &value
	}
	if imagesResult := gjson.GetBytes(body, "images"); imagesResult.Exists() {
		if !imagesResult.IsArray() {
			return nil, fmt.Errorf("images must be an array")
		}
		for _, item := range imagesResult.Array() {
			imageURL := strings.TrimSpace(item.Get("image_url").String())
			if imageURL != "" {
				parsed.InputImageURLs = append(parsed.InputImageURLs, imageURL)
			}
		}
	}
	if maskImageURL := strings.TrimSpace(gjson.GetBytes(body, "mask.image_url").String()); maskImageURL != "" {
		parsed.MaskImageURL = maskImageURL
	}
	if uploadResult := gjson.GetBytes(body, "image_files"); uploadResult.Exists() {
		if !uploadResult.IsArray() {
			return nil, fmt.Errorf("image_files must be an array")
		}
		uploadItems := uploadResult.Array()
		if len(uploadItems) > codexImageMaxUploads {
			return nil, fmt.Errorf("image edit supports at most %d images", codexImageMaxUploads)
		}
		for _, item := range uploadItems {
			upload := codexImageUpload{
				FileName:    strings.TrimSpace(item.Get("file_name").String()),
				ContentType: strings.TrimSpace(item.Get("content_type").String()),
				DataBase64:  strings.TrimSpace(item.Get("data_base64").String()),
				Width:       int(item.Get("width").Int()),
				Height:      int(item.Get("height").Int()),
			}
			if upload.FileName == "" {
				upload.FileName = "image.png"
			}
			if upload.ContentType == "" {
				upload.ContentType = "application/octet-stream"
			}
			if upload.DataBase64 == "" {
				return nil, fmt.Errorf("image_files[].data_base64 is required")
			}
			decoded, err := base64.StdEncoding.DecodeString(upload.DataBase64)
			if err != nil {
				return nil, fmt.Errorf("image_files[].data_base64 is invalid")
			}
			if len(decoded) == 0 {
				return nil, fmt.Errorf("image_files[].data_base64 is empty")
			}
			upload.Data = decoded
			parsed.Uploads = append(parsed.Uploads, upload)
		}
	}
	if maskResult := gjson.GetBytes(body, "mask_file"); maskResult.Exists() {
		maskUpload, err := parseCodexImageUpload(maskResult, "mask.png")
		if err != nil {
			return nil, err
		}
		parsed.MaskUpload = maskUpload
	}
	if len(parsed.Uploads) == 0 && len(parsed.InputImageURLs) == 0 && parsed.MaskUpload == nil && strings.TrimSpace(parsed.MaskImageURL) == "" && strings.TrimSpace(parsed.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if len(parsed.Uploads) == 0 && len(parsed.InputImageURLs) == 0 && parsed.MaskUpload == nil && strings.TrimSpace(parsed.MaskImageURL) == "" {
		return parsed, nil
	}
	if len(parsed.Uploads) == 0 && len(parsed.InputImageURLs) == 0 {
		return nil, fmt.Errorf("image input is required")
	}
	return parsed, nil
}

func parseCodexImageUpload(item gjson.Result, defaultFileName string) (*codexImageUpload, error) {
	if !item.Exists() {
		return nil, nil
	}
	upload := &codexImageUpload{
		FileName:    strings.TrimSpace(item.Get("file_name").String()),
		ContentType: strings.TrimSpace(item.Get("content_type").String()),
		DataBase64:  strings.TrimSpace(item.Get("data_base64").String()),
		Width:       int(item.Get("width").Int()),
		Height:      int(item.Get("height").Int()),
	}
	if upload.FileName == "" {
		upload.FileName = defaultFileName
	}
	if upload.ContentType == "" {
		upload.ContentType = "application/octet-stream"
	}
	if upload.DataBase64 == "" {
		return nil, fmt.Errorf("%s.data_base64 is required", strings.TrimSuffix(defaultFileName, ".png")+"_file")
	}
	decoded, err := base64.StdEncoding.DecodeString(upload.DataBase64)
	if err != nil {
		return nil, fmt.Errorf("%s.data_base64 is invalid", strings.TrimSuffix(defaultFileName, ".png")+"_file")
	}
	if len(decoded) == 0 {
		return nil, fmt.Errorf("%s.data_base64 is empty", strings.TrimSuffix(defaultFileName, ".png")+"_file")
	}
	upload.Data = decoded
	return upload, nil
}

func isValidCodexImageSize(size string) bool {
	return codexImageSizePattern.MatchString(strings.ToLower(strings.TrimSpace(size)))
}

// isSupportedCodexImageQuality accepts the quality levels the GPT Image family
// documents. xhigh, max and auto arrived with 2.5; rejecting them locally would
// fail a request this proxy has no reason to refuse, since the endpoint decides
// what it honours. (Today it honours none of them and echoes back "low".)
func isSupportedCodexImageQuality(quality string) bool {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low", "medium", "high", "xhigh", "max", "auto":
		return true
	default:
		return false
	}
}

func buildCodexImagePrompt(parsed *codexImageRequest, index int) string {
	if parsed == nil {
		return codexImageDefaultPrompt
	}
	base := strings.TrimSpace(parsed.Prompt)
	if base == "" {
		base = codexImageDefaultPrompt
	}
	extras := make([]string, 0, 8)
	extras = append(extras,
		"Generate an image that satisfies the user's request.",
		"Return only the final generated image and do not reply with chat text, questions, or explanations.",
	)
	if parsed.Size != "" {
		extras = append(extras, "Preferred image size: "+parsed.Size+".")
	}
	if parsed.Quality != "" {
		extras = append(extras, "Preferred render quality: "+parsed.Quality+".")
	}
	if parsed.N > 1 {
		extras = append(extras, fmt.Sprintf("This is variation %d of %d. Keep it distinct while following the same prompt.", index+1, parsed.N))
	}
	if len(parsed.Uploads) > 0 {
		extras = append(extras,
			"Use the attached image as the source reference and preserve the original composition unless the prompt says otherwise.",
			"Create a new edited image from the uploaded source image and apply the requested changes.",
			"Do not return the original uploaded image as the final output.",
			"Output only the edited result image.",
		)
	}
	return strings.Join(append(extras, "User request: "+base), "\n")
}

func buildCodexImageConversationRequest(prompt, parentMessageID string, uploads []codexUploadedImage) map[string]any {
	parts := []any{coalesceCodexImageText(prompt, codexImageDefaultPrompt)}
	contentType := "text"
	attachments := make([]map[string]any, 0, len(uploads))
	if len(uploads) > 0 {
		contentType = "multimodal_text"
		parts = make([]any, 0, len(uploads)+1)
		for _, upload := range uploads {
			parts = append(parts, map[string]any{
				"content_type":  "image_asset_pointer",
				"asset_pointer": "file-service://" + upload.FileID,
				"size_bytes":    upload.FileSize,
				"width":         upload.Width,
				"height":        upload.Height,
			})
			attachment := map[string]any{
				"id":       upload.FileID,
				"mimeType": upload.ContentType,
				"name":     upload.FileName,
				"size":     upload.FileSize,
			}
			if upload.Width > 0 {
				attachment["width"] = upload.Width
			}
			if upload.Height > 0 {
				attachment["height"] = upload.Height
			}
			attachments = append(attachments, attachment)
		}
		parts = append(parts, coalesceCodexImageText(prompt, "Edit this image."))
	}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []string{"picture_v2"},
		"serialization_metadata": map[string]any{
			"custom_symbol_offsets": []any{},
		},
	}
	message := map[string]any{
		"id":     uuid.NewString(),
		"author": map[string]any{"role": "user"},
		"content": map[string]any{
			"content_type": contentType,
			"parts":        parts,
		},
		"metadata":    metadata,
		"create_time": float64(time.Now().UnixMilli()) / 1000,
	}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	return map[string]any{
		"action":                               "next",
		"client_prepare_state":                 "sent",
		"parent_message_id":                    parentMessageID,
		"model":                                "auto",
		"timezone_offset_min":                  codexTimezoneOffsetMinutes(),
		"timezone":                             codexTimezoneName(),
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []string{"picture_v2"},
		"supports_buffering":                   true,
		"supported_encodings":                  []string{"v1"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_contextual_info": map[string]any{
			"is_dark_mode":      false,
			"time_since_loaded": 200,
			"page_height":       900,
			"page_width":        1440,
			"pixel_ratio":       1,
			"screen_height":     1080,
			"screen_width":      1920,
			"app_name":          "chatgpt.com",
		},
		"messages": []any{message},
	}
}

func codexImageCoalesce(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func sanitizeCodexImageRequestForLog(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return string(body)
	}
	sanitized := append([]byte(nil), body...)
	if gjson.GetBytes(sanitized, "image_files").Exists() {
		imageFiles := gjson.GetBytes(sanitized, "image_files").Array()
		for i, item := range imageFiles {
			size := 0
			if data := item.Get("data_base64").String(); data != "" {
				size = len(data)
			}
			replacement := map[string]any{
				"file_name":    strings.TrimSpace(item.Get("file_name").String()),
				"content_type": strings.TrimSpace(item.Get("content_type").String()),
				"data_base64":  fmt.Sprintf("[omitted:%d chars]", size),
			}
			if width := item.Get("width").Int(); width > 0 {
				replacement["width"] = width
			}
			if height := item.Get("height").Int(); height > 0 {
				replacement["height"] = height
			}
			if updated, err := sjson.SetBytes(sanitized, fmt.Sprintf("image_files.%d", i), replacement); err == nil {
				sanitized = updated
			}
		}
	}
	if maskFile := gjson.GetBytes(sanitized, "mask_file"); maskFile.Exists() {
		size := len(maskFile.Get("data_base64").String())
		replacement := map[string]any{
			"file_name":    strings.TrimSpace(maskFile.Get("file_name").String()),
			"content_type": strings.TrimSpace(maskFile.Get("content_type").String()),
			"data_base64":  fmt.Sprintf("[omitted:%d chars]", size),
		}
		if updated, err := sjson.SetBytes(sanitized, "mask_file", replacement); err == nil {
			sanitized = updated
		}
	}
	return string(sanitized)
}

func codexImageUploadToDataURL(upload codexImageUpload) (string, error) {
	contentType := strings.TrimSpace(upload.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(upload.Data) == 0 {
		return "", fmt.Errorf("upload %q is empty", codexImageCoalesce(upload.FileName, "image"))
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(upload.Data), nil
}

func coalesceCodexImageText(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func dedupeCodexImageStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

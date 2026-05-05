package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAIImageGenerationAlt = "images/generations"
	openAIImageEditsAlt      = "images/edits"
	openAIImageMaxUploadSize = 20 << 20
	openAIImageMaxN          = 4
)

type OpenAIImagesAPIHandler struct {
	*handlers.BaseAPIHandler
}

func NewOpenAIImagesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIImagesAPIHandler {
	return &OpenAIImagesAPIHandler{BaseAPIHandler: apiHandlers}
}

func (h *OpenAIImagesAPIHandler) Generations(c *gin.Context) {
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return
	}
	h.executeImages(c, rawJSON, openAIImageGenerationAlt)
}

func (h *OpenAIImagesAPIHandler) Edits(c *gin.Context) {
	rawJSON, ok := readOpenAIImageEditRequest(c)
	if !ok {
		return
	}
	h.executeImages(c, rawJSON, openAIImageEditsAlt)
}

func (h *OpenAIImagesAPIHandler) executeImages(c *gin.Context, rawJSON []byte, alt string) {
	modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if modelName == "" {
		modelName = openAIImageModelID
		if updated, err := sjson.SetBytes(rawJSON, "model", modelName); err == nil {
			rawJSON = updated
		}
	}
	imageCount, countErr := openAIImageRequestCount(rawJSON)
	if countErr != nil {
		writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error", countErr.Error())
		return
	}

	cliCtx := context.WithValue(c.Request.Context(), util.ContextKeyGin, c)
	if h.AuthManager == nil {
		writeOpenAIImagesError(c, http.StatusInternalServerError, "server_error", "authentication manager not initialized")
		return
	}

	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	defer stopKeepAlive()

	payloads := make([][]byte, 0, imageCount)
	var responseHeaders http.Header
	for i := 0; i < imageCount; i++ {
		execPayload := rawJSON
		if imageCount > 1 {
			var setErr error
			execPayload, setErr = sjson.SetBytes(rawJSON, "n", 1)
			if setErr != nil {
				writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error", "invalid image generation request")
				return
			}
		}
		payload, headers, errMsg := h.ExecuteWithAuthManager(cliCtx, "openai", modelName, execPayload, alt)
		if errMsg != nil {
			status := errMsg.StatusCode
			if status <= 0 {
				status = http.StatusBadGateway
			}
			message := ""
			if errMsg.Error != nil {
				message = errMsg.Error.Error()
			}
			writeOpenAIImagesError(c, status, errorTypeForStatus(status), message)
			return
		}
		if responseHeaders == nil {
			responseHeaders = headers
		}
		payloads = append(payloads, payload)
	}
	payload, err := mergeOpenAIImageResponses(payloads)
	if err != nil {
		writeOpenAIImagesError(c, http.StatusBadGateway, "server_error", err.Error())
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), responseHeaders)
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
}

func openAIImageRequestCount(rawJSON []byte) (int, error) {
	nResult := gjson.GetBytes(rawJSON, "n")
	if !nResult.Exists() {
		return 1, nil
	}
	if nResult.Type != gjson.Number {
		return 0, fmt.Errorf("n must be a number")
	}
	n := int(nResult.Int())
	if n < 1 || n > openAIImageMaxN {
		return 0, fmt.Errorf("n must be between 1 and %d", openAIImageMaxN)
	}
	return n, nil
}

func mergeOpenAIImageResponses(payloads [][]byte) ([]byte, error) {
	if len(payloads) == 0 {
		return nil, fmt.Errorf("image generation returned no responses")
	}
	if len(payloads) == 1 {
		return payloads[0], nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(payloads[0], &merged); err != nil {
		return nil, fmt.Errorf("parse image generation response: %w", err)
	}
	data := make([]json.RawMessage, 0, len(payloads))
	for _, payload := range payloads {
		var item struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, fmt.Errorf("parse image generation response: %w", err)
		}
		data = append(data, item.Data...)
	}
	encodedData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode image generation response: %w", err)
	}
	merged["data"] = encodedData
	return json.Marshal(merged)
}

func readOpenAIImageEditRequest(c *gin.Context) ([]byte, bool) {
	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		payload, parseErr := buildOpenAIImageEditPayloadFromMultipart(c)
		if parseErr != nil {
			writeOpenAIImagesError(c, http.StatusBadRequest, "invalid_request_error", parseErr.Error())
			return nil, false
		}
		return payload, true
	}
	rawJSON, ok := handlers.ReadJSONRequestBody(c)
	if !ok {
		return nil, false
	}
	return rawJSON, true
}

func buildOpenAIImageEditPayloadFromMultipart(c *gin.Context) ([]byte, error) {
	if err := c.Request.ParseMultipartForm(openAIImageMaxUploadSize); err != nil {
		return nil, fmt.Errorf("invalid multipart body")
	}
	form := c.Request.MultipartForm
	if form == nil {
		return nil, fmt.Errorf("invalid multipart body")
	}
	payload := map[string]any{
		"model":  firstOpenAIImagesFormValue(form.Value, "model", "gpt-image-2"),
		"prompt": firstOpenAIImagesFormValue(form.Value, "prompt", ""),
	}
	for _, field := range []string{"size", "quality", "response_format", "background", "output_format", "moderation", "input_fidelity", "style"} {
		if value := firstOpenAIImagesFormValue(form.Value, field, ""); value != "" {
			payload[field] = value
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := firstOpenAIImagesFormValue(form.Value, field, ""); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("%s must be a positive integer", field)
			}
			payload[field] = parsed
		}
	}
	if value := firstOpenAIImagesFormValue(form.Value, "n", ""); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("n must be a positive integer")
		}
		payload["n"] = n
	}
	files := form.File["image"]
	for key, value := range form.File {
		if strings.HasPrefix(key, "image[") {
			files = append(files, value...)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("image file is required")
	}
	uploads := make([]map[string]any, 0, len(files))
	for _, fileHeader := range files {
		if fileHeader == nil {
			continue
		}
		if fileHeader.Size > openAIImageMaxUploadSize {
			return nil, fmt.Errorf("image file is too large")
		}
		file, err := fileHeader.Open()
		if err != nil {
			return nil, fmt.Errorf("read image file: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(file, openAIImageMaxUploadSize+1))
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("read image file: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("image file is empty")
		}
		if len(data) > openAIImageMaxUploadSize {
			return nil, fmt.Errorf("image file is too large")
		}
		contentType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		uploads = append(uploads, map[string]any{
			"file_name":    fileHeader.Filename,
			"content_type": contentType,
			"data_base64":  base64.StdEncoding.EncodeToString(data),
		})
	}
	payload["image_files"] = uploads
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		maskFile, err := buildOpenAIImageUploadPayload(maskFiles[0])
		if err != nil {
			return nil, err
		}
		payload["mask_file"] = maskFile
	}
	return json.Marshal(payload)
}

func buildOpenAIImageUploadPayload(fileHeader *multipart.FileHeader) (map[string]any, error) {
	if fileHeader == nil {
		return nil, fmt.Errorf("image file is required")
	}
	if fileHeader.Size > openAIImageMaxUploadSize {
		return nil, fmt.Errorf("image file is too large")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, openAIImageMaxUploadSize+1))
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("read image file: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("image file is empty")
	}
	if len(data) > openAIImageMaxUploadSize {
		return nil, fmt.Errorf("image file is too large")
	}
	contentType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return map[string]any{
		"file_name":    fileHeader.Filename,
		"content_type": contentType,
		"data_base64":  base64.StdEncoding.EncodeToString(data),
	}, nil
}

func firstOpenAIImagesFormValue(values map[string][]string, key, fallback string) string {
	for _, value := range values[key] {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return fallback
}

func errorTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

func writeOpenAIImagesError(c *gin.Context, status int, errorType string, message string) {
	c.JSON(status, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: strings.TrimSpace(message),
			Type:    errorType,
		},
	})
}

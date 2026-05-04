package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/crypto/sha3"
)

const (
	codexImageModel              = "gpt-image-2"
	codexImageResponsesMainModel = "gpt-5.4-mini"
	codexImageBackendUserAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	codexImageRequirementsDiff   = "0fffff"
	codexImageGenerationAlt      = "images/generations"
	codexImageEditsAlt           = "images/edits"
	codexImageDefaultPrompt      = "Generate an image."
	codexImageConversationTimout = 5 * time.Minute
	codexImageMaxN               = 4
	codexImageMaxUploads         = 5
	codexImageResponsesMaxTries  = 3
	codexImageResponsesMaxRetry  = 2 * time.Second
)

var codexImageChatGPTBaseURL = "https://chatgpt.com"
var codexImagePollTimeout = codexImageConversationTimout
var codexImagePollInterval = 3 * time.Second
var codexImageSizePattern = regexp.MustCompile(`^[1-9][0-9]*x[1-9][0-9]*$`)

type codexImageRequest struct {
	Model             string
	Prompt            string
	N                 int
	Size              string
	Quality           string
	Background        string
	OutputFormat      string
	Moderation        string
	Style             string
	InputFidelity     string
	Stream            bool
	ResponseFormat    string
	OutputCompression *int
	PartialImages     *int
	InputImageURLs    []string
	MaskImageURL      string
	MaskUpload        *codexImageUpload
	Uploads           []codexImageUpload
}

type codexImageUpload struct {
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	DataBase64  string `json:"data_base64,omitempty"`
	Data        []byte `json:"-"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

type codexUploadedImage struct {
	FileID      string
	FileName    string
	FileSize    int
	ContentType string
	Width       int
	Height      int
}

type codexChatRequirements struct {
	Token  string `json:"token"`
	Arkose struct {
		Required bool `json:"required"`
	} `json:"arkose"`
	ProofOfWork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

type codexImagePointer struct {
	Pointer     string
	Prompt      string
	DownloadURL string
	B64JSON     string
	MimeType    string
}

type codexImageToolMessage struct {
	CreateTime float64
	Pointers   []codexImagePointer
}

func (r *codexImageRequest) hasEditInputs() bool {
	return r != nil && (len(r.Uploads) > 0 || len(r.InputImageURLs) > 0 || r.MaskUpload != nil || strings.TrimSpace(r.MaskImageURL) != "")
}

func (e *CodexExecutor) executeImageGeneration(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	parsed, err := parseCodexImageRequest(req.Payload)
	if err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	reporter := newUsageReporter(ctx, e.Identifier(), parsed.Model, auth)
	inputForLog := sanitizeCodexImageRequestForLog(req.Payload)
	reporter.setInputContent(inputForLog)
	defer reporter.trackFailure(ctx, &err)
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusUnauthorized, msg: "codex image generation requires a Codex OAuth access token"}
	}
	if opts.Alt == codexImageGenerationAlt || opts.Alt == codexImageEditsAlt || parsed.hasEditInputs() {
		payloads := make([][]byte, 0, parsed.N)
		var responseHeaders http.Header
		for i := 0; i < parsed.N; i++ {
			parsedOnce := *parsed
			parsedOnce.N = 1
			if parsed.N > 1 {
				parsedOnce.Prompt = buildCodexImagePrompt(parsed, i)
			}
			payload, headers, execErr := e.executeCodexImageViaResponses(ctx, auth, req.Payload, &parsedOnce)
			if execErr != nil {
				return cliproxyexecutor.Response{}, execErr
			}
			payloads = append(payloads, payload)
			if responseHeaders == nil {
				responseHeaders = headers
			}
		}
		payload, err := mergeCodexImageOpenAIResponses(payloads)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		reporter.publishWithContent(ctx, parseOpenAIUsage(payload), inputForLog, string(payload))
		reporter.ensurePublished(ctx)
		return cliproxyexecutor.Response{Payload: payload, Headers: responseHeaders}, nil
	}

	ctxRequest := ctx
	if ctxRequest == nil {
		ctxRequest = context.Background()
	}
	if _, ok := ctxRequest.Deadline(); !ok {
		var cancel context.CancelFunc
		ctxRequest, cancel = context.WithTimeout(ctxRequest, codexImageConversationTimout)
		defer cancel()
	}

	httpClient := newProxyAwareHTTPClient(ctxRequest, e.cfg, auth, codexImageConversationTimout)
	headers := buildCodexImageBackendHeaders(auth, apiKey)

	notifyCodexImagePhase(ctxRequest, "bootstrap")
	_ = codexImageBootstrap(ctxRequest, httpClient, headers)
	notifyCodexImagePhase(ctxRequest, "chat_requirements")
	chatReqs, err := fetchCodexImageChatRequirements(ctxRequest, httpClient, headers)
	if err != nil {
		return cliproxyexecutor.Response{}, wrapCodexImagePhaseError("chat-requirements", err)
	}
	if chatReqs.Arkose.Required {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusForbidden, msg: "chat-requirements requires unsupported challenge (arkose)"}
	}

	proofToken := generateCodexImageProofToken(chatReqs.ProofOfWork.Required, chatReqs.ProofOfWork.Seed, chatReqs.ProofOfWork.Difficulty, headers.Get("User-Agent"))
	payloads := make([][]byte, 0, parsed.N)
	var responseHeaders http.Header
	for i := 0; i < parsed.N; i++ {
		payload, responseHeader, runErr := e.executeCodexImageOnce(ctxRequest, httpClient, cloneHeader(headers), parsed, chatReqs, proofToken, i)
		if runErr != nil {
			return cliproxyexecutor.Response{}, runErr
		}
		payloads = append(payloads, payload)
		if responseHeaders == nil {
			responseHeaders = responseHeader
		}
	}

	payload, err := mergeCodexImageOpenAIResponses(payloads)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	reporter.publishWithContent(ctxRequest, parseOpenAIUsage(payload), inputForLog, string(payload))
	reporter.ensurePublished(ctxRequest)
	return cliproxyexecutor.Response{Payload: payload, Headers: responseHeaders}, nil
}

func (e *CodexExecutor) executeCodexImageOnce(
	ctx context.Context,
	httpClient *http.Client,
	headers http.Header,
	parsed *codexImageRequest,
	chatReqs *codexChatRequirements,
	proofToken string,
	index int,
) ([]byte, http.Header, error) {
	parentMessageID := uuid.NewString()
	prompt := buildCodexImagePrompt(parsed, index)
	notifyCodexImagePhase(ctx, "conversation_init")
	_ = initializeCodexImageConversation(ctx, httpClient, headers)
	notifyCodexImagePhase(ctx, "conversation_prepare")
	conduitToken, err := prepareCodexImageConversation(ctx, httpClient, headers, prompt, parentMessageID, chatReqs.Token, proofToken)
	if err != nil {
		return nil, nil, wrapCodexImagePhaseError("conversation prepare", err)
	}

	uploads, err := uploadCodexImageFiles(ctx, httpClient, headers, parsed.Uploads)
	if err != nil {
		return nil, nil, wrapCodexImagePhaseError("file upload", err)
	}

	notifyCodexImagePhase(ctx, "conversation_request")
	convReq := buildCodexImageConversationRequest(prompt, parentMessageID, uploads)
	convHeaders := cloneHeader(headers)
	convHeaders.Set("Accept", "text/event-stream")
	convHeaders.Set("Content-Type", "application/json")
	convHeaders.Set("openai-sentinel-chat-requirements-token", chatReqs.Token)
	if conduitToken != "" {
		convHeaders.Set("x-conduit-token", conduitToken)
	}
	if proofToken != "" {
		convHeaders.Set("openai-sentinel-proof-token", proofToken)
	}

	body, _ := json.Marshal(convReq)
	httpResp, err := doCodexImageJSON(ctx, httpClient, http.MethodPost, codexImageURL("/backend-api/f/conversation"), convHeaders, body)
	if err != nil {
		return nil, nil, wrapCodexImagePhaseError("conversation request", err)
	}
	defer func() {
		if httpResp != nil && httpResp.Body != nil {
			_ = httpResp.Body.Close()
		}
	}()
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, codexImageStatusErr(httpResp, "openai image conversation request failed")
	}

	notifyCodexImagePhase(ctx, "conversation_stream")
	conversationID, pointers, err := readCodexImageConversationStream(httpResp.Body)
	if err != nil {
		return nil, nil, wrapCodexImagePhaseError("conversation stream", err)
	}
	if conversationID != "" && len(pointers) == 0 {
		notifyCodexImagePhase(ctx, "conversation_poll")
		polled, pollErr := pollCodexImageConversation(ctx, httpClient, headers, conversationID)
		if pollErr != nil {
			return nil, nil, wrapCodexImagePhaseError("conversation poll", pollErr)
		}
		pointers = mergeCodexImagePointers(pointers, polled)
	}
	pointers = preferCodexFileServicePointers(pointers)
	if len(pointers) == 0 {
		return nil, nil, statusErr{code: http.StatusBadGateway, msg: "openai image conversation returned no downloadable images"}
	}

	notifyCodexImagePhase(ctx, "image_download")
	payload, err := buildCodexImageOpenAIResponse(ctx, httpClient, headers, conversationID, pointers)
	if err != nil {
		return nil, nil, wrapCodexImagePhaseError("image download", err)
	}
	return payload, httpResp.Header.Clone(), nil
}

func notifyCodexImagePhase(ctx context.Context, phase string) {
	if ctx == nil {
		return
	}
	hook, _ := ctx.Value(util.ContextKeyImageGenerationPhaseHook).(func(string))
	if hook != nil {
		hook(strings.TrimSpace(phase))
	}
}

func wrapCodexImagePhaseError(phase string, err error) error {
	if err == nil {
		return nil
	}
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return err
	}
	if status, ok := err.(statusErr); ok {
		if !strings.Contains(status.msg, phase) {
			status.msg = phase + ": " + status.msg
		}
		return status
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "request failed"
	}
	if strings.Contains(message, phase) {
		return err
	}
	return fmt.Errorf("%s: %w", phase, err)
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
	if model != codexImageModel {
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
		return nil, fmt.Errorf("quality must be one of low, medium, high")
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

func isSupportedCodexImageQuality(quality string) bool {
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

func buildCodexImageBackendHeaders(auth *cliproxyauth.Auth, token string) http.Header {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Accept", "application/json")
	headers.Set("Origin", "https://chatgpt.com")
	headers.Set("Referer", "https://chatgpt.com/")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", "same-origin")
	headers.Set("User-Agent", codexImageBackendUserAgent)
	if auth != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			headers.Set("chatgpt-account-id", strings.TrimSpace(accountID))
		}
	}
	deviceID, sessionID := codexImageSessionIDs(auth)
	if deviceID != "" {
		headers.Set("oai-device-id", deviceID)
		headers.Set("Cookie", "oai-did="+deviceID)
	}
	if sessionID != "" {
		headers.Set("oai-session-id", sessionID)
	}
	return headers
}

func codexImageSessionIDs(auth *cliproxyauth.Auth) (string, string) {
	deviceID := ""
	sessionID := ""
	if auth != nil && auth.Metadata != nil {
		if raw, ok := auth.Metadata["openai_device_id"].(string); ok {
			deviceID = strings.TrimSpace(raw)
		}
		if raw, ok := auth.Metadata["openai_session_id"].(string); ok {
			sessionID = strings.TrimSpace(raw)
		}
	}
	if deviceID == "" {
		deviceID = uuid.NewString()
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	return deviceID, sessionID
}

func codexImageURL(path string) string {
	base := strings.TrimRight(strings.TrimSpace(codexImageChatGPTBaseURL), "/")
	if base == "" {
		base = "https://chatgpt.com"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func codexImageBootstrap(ctx context.Context, client *http.Client, headers http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexImageURL("/"), nil)
	if err != nil {
		return err
	}
	req.Header = cloneHeader(headers)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func fetchCodexImageChatRequirements(ctx context.Context, client *http.Client, headers http.Header) (*codexChatRequirements, error) {
	var lastErr error
	for _, payload := range []map[string]any{
		{"p": nil},
		{"p": generateCodexImageRequirementsToken(headers.Get("User-Agent"))},
	} {
		body, _ := json.Marshal(payload)
		resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/sentinel/chat-requirements"), headers, body)
		if err != nil {
			lastErr = err
			continue
		}
		respBody, readErr := readAndCloseCodexImageBody(resp)
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			var result codexChatRequirements
			if err := json.Unmarshal(respBody, &result); err != nil {
				lastErr = err
				continue
			}
			if strings.TrimSpace(result.Token) != "" {
				return &result, nil
			}
		}
		lastErr = codexImageStatusErrWithBody(resp.StatusCode, respBody, "chat-requirements failed")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("chat-requirements failed")
	}
	return nil, lastErr
}

func initializeCodexImageConversation(ctx context.Context, client *http.Client, headers http.Header) error {
	payload := map[string]any{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     codexTimezoneOffsetMinutes(),
		"system_hints":            []string{"picture_v2"},
	}
	body, _ := json.Marshal(payload)
	resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/conversation/init"), headers, body)
	if err != nil {
		return err
	}
	respBody, readErr := readAndCloseCodexImageBody(resp)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return codexImageStatusErrWithBody(resp.StatusCode, respBody, "conversation init failed")
	}
	return nil
}

func prepareCodexImageConversation(ctx context.Context, client *http.Client, headers http.Header, prompt, parentMessageID, chatToken, proofToken string) (string, error) {
	messageID := uuid.NewString()
	payload := map[string]any{
		"action":                "next",
		"client_prepare_state":  "success",
		"fork_from_shared_post": false,
		"parent_message_id":     parentMessageID,
		"model":                 "auto",
		"timezone_offset_min":   codexTimezoneOffsetMinutes(),
		"timezone":              codexTimezoneName(),
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []string{"picture_v2"},
		"supports_buffering":    true,
		"supported_encodings":   []string{"v1"},
		"partial_query": map[string]any{
			"id":     messageID,
			"author": map[string]any{"role": "user"},
			"content": map[string]any{
				"content_type": "text",
				"parts":        []string{coalesceCodexImageText(prompt, codexImageDefaultPrompt)},
			},
		},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	prepareHeaders := cloneHeader(headers)
	prepareHeaders.Set("Accept", "*/*")
	prepareHeaders.Set("Content-Type", "application/json")
	if strings.TrimSpace(chatToken) != "" {
		prepareHeaders.Set("openai-sentinel-chat-requirements-token", strings.TrimSpace(chatToken))
	}
	if strings.TrimSpace(proofToken) != "" {
		prepareHeaders.Set("openai-sentinel-proof-token", strings.TrimSpace(proofToken))
	}
	body, _ := json.Marshal(payload)
	resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/f/conversation/prepare"), prepareHeaders, body)
	if err != nil {
		return "", err
	}
	respBody, readErr := readAndCloseCodexImageBody(resp)
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", codexImageStatusErrWithBody(resp.StatusCode, respBody, "conversation prepare failed")
	}
	var result struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", err
	}
	return strings.TrimSpace(result.ConduitToken), nil
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

func uploadCodexImageFiles(ctx context.Context, client *http.Client, headers http.Header, uploads []codexImageUpload) ([]codexUploadedImage, error) {
	if len(uploads) == 0 {
		return nil, nil
	}
	results := make([]codexUploadedImage, 0, len(uploads))
	for _, item := range uploads {
		payload, _ := json.Marshal(map[string]any{
			"file_name": codexImageCoalesce(item.FileName, "image.png"),
			"file_size": len(item.Data),
			"use_case":  "multimodal",
		})
		resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/files"), headers, payload)
		if err != nil {
			return nil, err
		}
		respBody, readErr := readAndCloseCodexImageBody(resp)
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, codexImageStatusErrWithBody(resp.StatusCode, respBody, "create upload slot failed")
		}
		var created struct {
			FileID    string `json:"file_id"`
			UploadURL string `json:"upload_url"`
		}
		if err := json.Unmarshal(respBody, &created); err != nil {
			return nil, err
		}
		if strings.TrimSpace(created.FileID) == "" || strings.TrimSpace(created.UploadURL) == "" {
			return nil, statusErr{code: http.StatusBadGateway, msg: "create upload slot failed"}
		}

		putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, created.UploadURL, bytes.NewReader(item.Data))
		if err != nil {
			return nil, err
		}
		putReq.Header.Set("Content-Type", codexImageCoalesce(item.ContentType, "application/octet-stream"))
		putReq.Header.Set("Origin", "https://chatgpt.com")
		putReq.Header.Set("x-ms-blob-type", "BlockBlob")
		putReq.Header.Set("x-ms-version", "2020-04-08")
		putReq.Header.Set("User-Agent", headers.Get("User-Agent"))
		putResp, err := client.Do(putReq)
		if err != nil {
			return nil, err
		}
		putBody, readErr := readAndCloseCodexImageBody(putResp)
		if readErr != nil {
			return nil, readErr
		}
		if putResp.StatusCode < http.StatusOK || putResp.StatusCode >= http.StatusMultipleChoices {
			return nil, codexImageStatusErrWithBody(putResp.StatusCode, putBody, "upload image bytes failed")
		}

		donePayload, _ := json.Marshal(map[string]any{})
		doneResp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/files/"+created.FileID+"/uploaded"), headers, donePayload)
		if err != nil {
			return nil, err
		}
		doneBody, readErr := readAndCloseCodexImageBody(doneResp)
		if readErr != nil {
			return nil, readErr
		}
		if doneResp.StatusCode < http.StatusOK || doneResp.StatusCode >= http.StatusMultipleChoices {
			return nil, codexImageStatusErrWithBody(doneResp.StatusCode, doneBody, "mark upload complete failed")
		}

		results = append(results, codexUploadedImage{
			FileID:      created.FileID,
			FileName:    codexImageCoalesce(item.FileName, "image.png"),
			FileSize:    len(item.Data),
			ContentType: codexImageCoalesce(item.ContentType, "application/octet-stream"),
			Width:       item.Width,
			Height:      item.Height,
		})
	}
	return results, nil
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

func mergeCodexImageOpenAIResponses(payloads [][]byte) ([]byte, error) {
	if len(payloads) == 0 {
		return nil, fmt.Errorf("no image payloads to merge")
	}
	if len(payloads) == 1 {
		return payloads[0], nil
	}
	type imageItem struct {
		B64JSON       string `json:"b64_json,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	merged := struct {
		Created int64       `json:"created"`
		Data    []imageItem `json:"data"`
	}{}
	for _, payload := range payloads {
		var item struct {
			Created int64       `json:"created"`
			Data    []imageItem `json:"data"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, err
		}
		if item.Created > merged.Created {
			merged.Created = item.Created
		}
		merged.Data = append(merged.Data, item.Data...)
	}
	return json.Marshal(merged)
}

type codexResponsesImageResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	Background    string
	Quality       string
}

func (e *CodexExecutor) executeCodexImageViaResponses(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	originalPayload []byte,
	parsed *codexImageRequest,
) ([]byte, http.Header, error) {
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	body, err := buildCodexImageResponsesRequest(parsed, codexImageModel)
	if err != nil {
		return nil, nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	var lastErr error
	for attempt := 1; attempt <= codexImageResponsesMaxTries; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		applyCodexHeaders(httpReq, e.cfg, auth, apiKey, true)
		recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
			URL:       url,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      body,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, err := httpClient.Do(httpReq)
		if err != nil {
			recordAPIResponseError(ctx, e.cfg, err)
			return nil, nil, err
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			upstreamBody := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
			_ = httpResp.Body.Close()
			appendAPIResponseChunk(ctx, e.cfg, upstreamBody)
			return nil, nil, newCodexStatusErr(httpResp.StatusCode, upstreamBody)
		}

		rawBody, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
		responseHeaders := httpResp.Header.Clone()
		_ = httpResp.Body.Close()
		if err != nil {
			recordAPIResponseError(ctx, e.cfg, err)
			return nil, nil, err
		}
		appendAPIResponseChunk(ctx, e.cfg, rawBody)

		results, createdAt, err := collectCodexImagesFromResponsesBody(rawBody)
		if err != nil {
			lastErr = err
			if retryDelay, ok := codexImageResponsesRetryDelay(err, attempt); ok {
				if waitErr := waitCodexImageResponsesRetry(ctx, retryDelay); waitErr != nil {
					return nil, nil, waitErr
				}
				continue
			}
			return nil, nil, err
		}
		if len(results) == 0 {
			return nil, nil, statusErr{code: http.StatusBadGateway, msg: "responses image request returned no generated images"}
		}
		payload, err := buildCodexImageOpenAIResponseFromResults(results, createdAt)
		if err != nil {
			return nil, nil, err
		}
		_ = originalPayload
		return payload, responseHeaders, nil
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, statusErr{code: http.StatusBadGateway, msg: "responses image request failed"}
}

func buildCodexImageResponsesRequest(parsed *codexImageRequest, toolModel string) ([]byte, error) {
	if parsed == nil {
		return nil, fmt.Errorf("parsed images request is required")
	}
	prompt := strings.TrimSpace(parsed.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	inputImages := make([]string, 0, len(parsed.InputImageURLs)+len(parsed.Uploads))
	for _, imageURL := range parsed.InputImageURLs {
		if trimmed := strings.TrimSpace(imageURL); trimmed != "" {
			inputImages = append(inputImages, trimmed)
		}
	}
	for _, upload := range parsed.Uploads {
		dataURL, err := codexImageUploadToDataURL(upload)
		if err != nil {
			return nil, err
		}
		inputImages = append(inputImages, dataURL)
	}
	if parsed.hasEditInputs() && len(inputImages) == 0 {
		return nil, fmt.Errorf("image input is required")
	}

	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	req, _ = sjson.SetBytes(req, "model", codexImageResponsesMainModel)

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	for index, imageURL := range inputImages {
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", imageURL)
		input, _ = sjson.SetRawBytes(input, fmt.Sprintf("0.content.%d", index+1), part)
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	action := "generate"
	if parsed.hasEditInputs() {
		action = "edit"
	}
	tool := []byte(`{"type":"image_generation","action":"","model":""}`)
	tool, _ = sjson.SetBytes(tool, "action", action)
	tool, _ = sjson.SetBytes(tool, "model", strings.TrimSpace(toolModel))
	for _, field := range []struct {
		path  string
		value string
	}{
		{path: "size", value: parsed.Size},
		{path: "quality", value: parsed.Quality},
		{path: "background", value: parsed.Background},
		{path: "output_format", value: parsed.OutputFormat},
		{path: "moderation", value: parsed.Moderation},
		{path: "style", value: parsed.Style},
	} {
		if trimmed := strings.TrimSpace(field.value); trimmed != "" {
			tool, _ = sjson.SetBytes(tool, field.path, trimmed)
		}
	}
	if parsed.OutputCompression != nil {
		tool, _ = sjson.SetBytes(tool, "output_compression", *parsed.OutputCompression)
	}
	if parsed.PartialImages != nil {
		tool, _ = sjson.SetBytes(tool, "partial_images", *parsed.PartialImages)
	}
	maskImageURL := strings.TrimSpace(parsed.MaskImageURL)
	if parsed.MaskUpload != nil {
		dataURL, err := codexImageUploadToDataURL(*parsed.MaskUpload)
		if err != nil {
			return nil, err
		}
		maskImageURL = dataURL
	}
	if maskImageURL != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", maskImageURL)
	}
	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	req, _ = sjson.SetRawBytes(req, "tools.-1", tool)
	return req, nil
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

func collectCodexImagesFromResponsesBody(body []byte) ([]codexResponsesImageResult, int64, error) {
	var (
		fallbackResults []codexResponsesImageResult
		fallbackSeen    = make(map[string]struct{})
		createdAt       int64
		foundFinal      bool
		streamErr       error
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		data, ok := codexExtractSSEDataLine(string(line))
		if !ok || data == "" || data == "[DONE]" {
			continue
		}
		payload := []byte(data)
		if !gjson.ValidBytes(payload) {
			continue
		}
		eventType := gjson.GetBytes(payload, "type").String()
		switch eventType {
		case "error":
			streamErr = codexResponsesFailedStatusErr(payload)
		case "response.output_item.done":
			result, itemID, ok := extractCodexImageFromResponsesOutputItemDone(payload)
			if !ok {
				continue
			}
			key := itemID + "|" + result.Result
			if _, exists := fallbackSeen[key]; exists {
				continue
			}
			fallbackSeen[key] = struct{}{}
			fallbackResults = append(fallbackResults, result)
		case "response.completed":
			results, completedAt, err := extractCodexImagesFromResponsesCompleted(payload)
			if err != nil {
				return nil, 0, err
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			if len(results) > 0 {
				return results, createdAt, nil
			}
			foundFinal = true
		case "response.failed":
			return nil, createdAt, codexResponsesFailedStatusErr(payload)
		case "response.created":
			if createdAt == 0 {
				createdAt = gjson.GetBytes(payload, "response.created_at").Int()
			}
		}
	}
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	if len(fallbackResults) > 0 {
		return fallbackResults, createdAt, nil
	}
	if streamErr != nil {
		return nil, createdAt, streamErr
	}
	if foundFinal {
		return nil, createdAt, nil
	}
	return nil, createdAt, fmt.Errorf("stream disconnected before response.completed")
}

func codexResponsesFailedStatusErr(payload []byte) statusErr {
	message := strings.TrimSpace(gjson.GetBytes(payload, "response.error.message").String())
	if message == "" {
		message = strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	}
	code := strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(payload, "error.code").String())
	}
	errType := strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String())
	if errType == "" {
		errType = strings.TrimSpace(gjson.GetBytes(payload, "error.type").String())
	}
	statusCode := http.StatusBadGateway
	if strings.Contains(code, "rate_limit") || strings.Contains(errType, "rate_limit") {
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
	}
	if message == "" {
		message = "responses image request failed"
	}
	if errType == "" {
		errType = "upstream_error"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	})
	err := statusErr{code: statusCode, msg: string(body), upstreamBody: body}
	if retryAfter := codexImageResponsesRetryAfter(message); retryAfter > 0 {
		err.retryAfter = &retryAfter
	}
	return err
}

func codexImageResponsesRetryDelay(err error, attempt int) (time.Duration, bool) {
	if attempt >= codexImageResponsesMaxTries {
		return 0, false
	}
	status, ok := err.(statusErr)
	if !ok || status.code != http.StatusTooManyRequests || status.retryAfter == nil {
		return 0, false
	}
	if *status.retryAfter <= 0 || *status.retryAfter > codexImageResponsesMaxRetry {
		return 0, false
	}
	return *status.retryAfter, true
}

func waitCodexImageResponsesRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var codexImageRetryAfterPattern = regexp.MustCompile(`(?i)try again in\s+([0-9]+(?:\.[0-9]+)?\s*(?:ms|s|sec|secs|second|seconds|m|min|mins|minute|minutes))`)

func codexImageResponsesRetryAfter(message string) time.Duration {
	match := codexImageRetryAfterPattern.FindStringSubmatch(message)
	if len(match) < 2 {
		return 0
	}
	value := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(match[1]), " ", ""))
	replacements := []struct {
		suffix string
		with   string
	}{
		{suffix: "seconds", with: "s"},
		{suffix: "second", with: "s"},
		{suffix: "secs", with: "s"},
		{suffix: "sec", with: "s"},
		{suffix: "minutes", with: "m"},
		{suffix: "minute", with: "m"},
		{suffix: "mins", with: "m"},
		{suffix: "min", with: "m"},
	}
	for _, replacement := range replacements {
		if strings.HasSuffix(value, replacement.suffix) {
			value = strings.TrimSuffix(value, replacement.suffix) + replacement.with
			break
		}
	}
	delay, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return delay
}

func extractCodexImagesFromResponsesCompleted(payload []byte) ([]codexResponsesImageResult, int64, error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, fmt.Errorf("unexpected event type")
	}
	createdAt := gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	results := make([]codexResponsesImageResult, 0, 1)
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			result := strings.TrimSpace(item.Get("result").String())
			if result == "" {
				continue
			}
			results = append(results, codexResponsesImageResult{
				Result:        result,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
			})
		}
	}
	return results, createdAt, nil
}

func extractCodexImageFromResponsesOutputItemDone(payload []byte) (codexResponsesImageResult, string, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return codexResponsesImageResult{}, "", false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Get("type").String() != "image_generation_call" {
		return codexResponsesImageResult{}, "", false
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return codexResponsesImageResult{}, "", false
	}
	return codexResponsesImageResult{
		Result:        result,
		RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
		Size:          strings.TrimSpace(item.Get("size").String()),
		Background:    strings.TrimSpace(item.Get("background").String()),
		Quality:       strings.TrimSpace(item.Get("quality").String()),
	}, strings.TrimSpace(item.Get("id").String()), true
}

func buildCodexImageOpenAIResponseFromResults(results []codexResponsesImageResult, createdAt int64) ([]byte, error) {
	type responseItem struct {
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	items := make([]responseItem, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.Result) == "" {
			continue
		}
		items = append(items, responseItem{
			B64JSON:       strings.TrimSpace(result.Result),
			RevisedPrompt: strings.TrimSpace(result.RevisedPrompt),
		})
	}
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	return json.Marshal(map[string]any{
		"created": createdAt,
		"data":    items,
	})
}

func doCodexImageJSON(ctx context.Context, client *http.Client, method, url string, headers http.Header, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = cloneHeader(headers)
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

func cloneHeader(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func readAndCloseCodexImageBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return readUpstreamResponseBody("codex-image", resp.Body)
}

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
		resp, err := doCodexImageJSON(ctx, client, http.MethodGet, codexImageURL("/backend-api/conversation/"+conversationID), headers, nil)
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
	resp, err := doCodexImageJSON(ctx, client, http.MethodGet, url, headers, nil)
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

func codexTimezoneOffsetMinutes() int {
	_, offset := time.Now().Zone()
	return offset / 60
}

func codexTimezoneName() string {
	return time.Now().Location().String()
}

func generateCodexImageRequirementsToken(userAgent string) string {
	config := []any{
		"core" + strconv.Itoa(3008),
		time.Now().UTC().Format(time.RFC1123),
		nil,
		0.123456,
		coalesceCodexImageText(strings.TrimSpace(userAgent), codexImageBackendUserAgent),
		nil,
		"prod-openai-images",
		"en-US",
		"en-US,en",
		0,
		"navigator.webdriver",
		"location",
		"document.body",
		float64(time.Now().UnixMilli()) / 1000,
		uuid.NewString(),
		"",
		8,
		time.Now().Unix(),
	}
	answer, solved := generateCodexImageChallengeAnswer(strconv.FormatInt(time.Now().UnixNano(), 10), codexImageRequirementsDiff, config)
	if solved {
		return "gAAAAAC" + answer
	}
	return ""
}

func generateCodexImageChallengeAnswer(seed string, difficulty string, config []any) (string, bool) {
	diffBytes, err := hex.DecodeString(difficulty)
	if err != nil {
		return "", false
	}
	p1 := []byte(jsonCompactCodexImageSlice(config[:3], true))
	p2 := []byte(jsonCompactCodexImageSlice(config[4:9], false))
	p3 := []byte(jsonCompactCodexImageSlice(config[10:], false))
	seedBytes := []byte(seed)
	for i := 0; i < 100000; i++ {
		payload := fmt.Sprintf("%s%d,%s,%d,%s", p1, i, p2, i>>1, p3)
		encoded := base64.StdEncoding.EncodeToString([]byte(payload))
		sum := sha3.Sum512(append(seedBytes, []byte(encoded)...))
		if bytes.Compare(sum[:len(diffBytes)], diffBytes) <= 0 {
			return encoded, true
		}
	}
	return "", false
}

func jsonCompactCodexImageSlice(values []any, trimSuffixComma bool) string {
	raw, _ := json.Marshal(values)
	text := string(raw)
	if trimSuffixComma {
		return strings.TrimSuffix(text, "]")
	}
	return strings.TrimPrefix(text, "[")
}

func generateCodexImageProofToken(required bool, seed string, difficulty string, userAgent string) string {
	if !required || strings.TrimSpace(seed) == "" || strings.TrimSpace(difficulty) == "" {
		return ""
	}
	screen := 3008
	if len(seed)%2 == 0 {
		screen = 4010
	}
	proofToken := []any{
		screen,
		time.Now().UTC().Format(time.RFC1123),
		nil,
		0,
		coalesceCodexImageText(strings.TrimSpace(userAgent), codexImageBackendUserAgent),
		"https://chatgpt.com/",
		"dpl=openai-images",
		"en",
		"en-US",
		nil,
		"plugins[object PluginArray]",
		"_reactListening",
		"alert",
	}
	diffLen := len(difficulty)
	for i := 0; i < 100000; i++ {
		proofToken[3] = i
		raw, _ := json.Marshal(proofToken)
		encoded := base64.StdEncoding.EncodeToString(raw)
		sum := sha3.Sum512([]byte(seed + encoded))
		if strings.Compare(hex.EncodeToString(sum[:])[:diffLen], difficulty) <= 0 {
			return "gAAAAAB" + encoded
		}
	}
	fallbackBase := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%q", seed)))
	return "gAAAAABwQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + fallbackBase
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

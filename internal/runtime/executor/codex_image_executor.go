package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
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

func (e *CodexExecutor) newCodexImageExecutionContext(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	parsed *codexImageRequest,
) *ExecutionContext {
	reqCopy := req
	if parsed != nil && strings.TrimSpace(parsed.Model) != "" {
		reqCopy.Model = parsed.Model
	}
	return newExecutionContext(ctx, e.Identifier(), e.cfg, auth, reqCopy, opts, ExecutionOptions{})
}

func (e *CodexExecutor) executeImageGeneration(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	parsed, err := parseCodexImageRequest(req.Payload)
	if err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: err.Error()}
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
	execCtx := e.newCodexImageExecutionContext(ctxRequest, auth, req, opts, parsed)
	reporter := execCtx.Reporter()
	inputForLog := sanitizeCodexImageRequestForLog(req.Payload)
	reporter.setInputContent(inputForLog)
	defer reporter.trackFailure(execCtx.Context, &err)
	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return cliproxyexecutor.Response{}, errAdmission
	}
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
			payload, headers, execErr := e.executeCodexImageViaResponses(execCtx, &parsedOnce)
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
		reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(payload), inputForLog, string(payload))
		reporter.ensurePublished(execCtx.Context)
		return cliproxyexecutor.Response{Payload: payload, Headers: responseHeaders}, nil
	}

	httpClient := execCtx.HTTPClient(codexImageConversationTimout)
	headers := buildCodexImageBackendHeaders(auth, apiKey)

	notifyCodexImagePhase(execCtx.Context, "bootstrap")
	_ = codexImageBootstrap(execCtx.Context, httpClient, headers)
	notifyCodexImagePhase(execCtx.Context, "chat_requirements")
	chatReqs, err := fetchCodexImageChatRequirements(execCtx.Context, httpClient, headers)
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
		payload, responseHeader, runErr := e.executeCodexImageOnce(execCtx.Context, httpClient, cloneHeader(headers), parsed, chatReqs, proofToken, i)
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
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(payload), inputForLog, string(payload))
	reporter.ensurePublished(execCtx.Context)
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

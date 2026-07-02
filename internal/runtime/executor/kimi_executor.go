package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	kimiauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KimiExecutor is a stateless executor for Kimi API using OpenAI-compatible chat completions.
type KimiExecutor struct {
	ClaudeExecutor
	cfg *config.Config
}

// NewKimiExecutor creates a new Kimi executor.
func NewKimiExecutor(cfg *config.Config) *KimiExecutor { return &KimiExecutor{cfg: cfg} }

// Identifier returns the executor identifier.
func (e *KimiExecutor) Identifier() string { return "kimi" }

// PrepareRequest injects Kimi credentials into the outgoing HTTP request.
func (e *KimiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := kimiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Kimi credentials into the request and executes it.
func (e *KimiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kimi executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming chat completion request to Kimi.
func (e *KimiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		return e.ClaudeExecutor.Execute(ctx, auth, req, opts)
	}

	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: false,
	})

	token := kimiCreds(auth)

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	// Strip kimi- prefix for upstream API
	upstreamModel := stripKimiPrefix(execCtx.BaseModel)
	translated, err = sjson.SetBytes(translated, "model", upstreamModel)
	if err != nil {
		return resp, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		"kimi",
		e.Identifier(),
	)
	if err != nil {
		return resp, err
	}

	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	translated, err = normalizeKimiToolMessageLinks(translated)
	if err != nil {
		return resp, err
	}

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	applyKimiHeadersWithAuth(httpReq, token, false, auth, e.cfg)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
	}()
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := readUpstreamResponseBody(e.Identifier(), httpResp.Body)
	if err != nil {
		recorder.RecordResponseError(err)
		return resp, err
	}
	recorder.AppendResponseChunk(data)
	reporter.publishWithContent(execCtx.Context, parseOpenAIUsage(data), string(req.Payload), string(data))
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(
		execCtx.Context,
		execCtx.Execution.TargetFormat,
		execCtx.SourceFormat,
		req.Model,
		opts.OriginalRequest,
		translated,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Kimi.
func (e *KimiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		return e.ClaudeExecutor.ExecuteStream(ctx, auth, req, opts)
	}

	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("openai"),
		TranslateAsStream: true,
	})
	token := kimiCreds(auth)

	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	translated, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	// Strip kimi- prefix for upstream API
	upstreamModel := stripKimiPrefix(execCtx.BaseModel)
	translated, err = sjson.SetBytes(translated, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	translated, err = thinking.ApplyThinking(
		translated,
		req.Model,
		execCtx.SourceFormat.String(),
		"kimi",
		e.Identifier(),
	)
	if err != nil {
		return nil, err
	}

	translated, err = sjson.SetBytes(translated, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set stream_options in payload: %w", err)
	}
	translated = execCtx.ApplyPayloadConfig(translated, originalTranslated)
	translated, err = normalizeKimiToolMessageLinks(translated)
	if err != nil {
		return nil, err
	}

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(execCtx.Context, http.MethodPost, url, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	applyKimiHeadersWithAuth(httpReq, token, true, auth, e.cfg)
	recorder := execCtx.Recorder()
	recorder.RecordRequest(url, http.MethodPost, httpReq.Header.Clone(), translated)

	httpClient := execCtx.HTTPClient(0)
	//nolint:bodyclose // success body is consumed and closed by the stream goroutine below.
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recorder.RecordResponseError(err)
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), err.Error())
		return nil, err
	}
	recorder.RecordResponseMetadata(httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b := readUpstreamErrorBody(e.Identifier(), httpResp.Body)
		recorder.AppendResponseChunk(b)
		logWithRequestID(execCtx.Context).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		reporter.publishFailureWithContent(execCtx.Context, string(req.Payload), string(b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	reporter.setInputContent(string(req.Payload))
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kimi executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			recorder.AppendResponseChunk(line)
			reporter.appendOutputChunk(line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(execCtx.Context, detail)
			}
			chunks := sdktranslator.TranslateStream(
				execCtx.Context,
				execCtx.Execution.TargetFormat,
				execCtx.SourceFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		doneChunks := sdktranslator.TranslateStream(
			execCtx.Context,
			execCtx.Execution.TargetFormat,
			execCtx.SourceFormat,
			req.Model,
			opts.OriginalRequest,
			translated,
			[]byte("[DONE]"),
			&param,
		)
		for i := range doneChunks {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(doneChunks[i])}
		}
		if errScan := scanner.Err(); errScan != nil {
			recorder.RecordResponseError(errScan)
			reporter.publishFailure(execCtx.Context)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Kimi requests.
func (e *KimiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
	return e.ClaudeExecutor.CountTokens(ctx, auth, req, opts)
}

func normalizeKimiToolMessageLinks(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	out := body
	pending := make([]string, 0)
	patched := 0
	patchedReasoning := 0
	ambiguous := 0
	latestReasoning := ""
	hasLatestReasoning := false

	removePending := func(id string) {
		for idx := range pending {
			if pending[idx] != id {
				continue
			}
			pending = append(pending[:idx], pending[idx+1:]...)
			return
		}
	}

	msgs := messages.Array()
	for msgIdx := range msgs {
		msg := msgs[msgIdx]
		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			reasoning := msg.Get("reasoning_content")
			if reasoning.Exists() {
				reasoningText := reasoning.String()
				if strings.TrimSpace(reasoningText) != "" {
					latestReasoning = reasoningText
					hasLatestReasoning = true
				}
			}

			toolCalls := msg.Get("tool_calls")
			if !toolCalls.Exists() || !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
				continue
			}

			if !reasoning.Exists() || strings.TrimSpace(reasoning.String()) == "" {
				reasoningText := fallbackAssistantReasoning(msg, hasLatestReasoning, latestReasoning)
				path := fmt.Sprintf("messages.%d.reasoning_content", msgIdx)
				next, err := sjson.SetBytes(out, path, reasoningText)
				if err != nil {
					return body, fmt.Errorf("kimi executor: failed to set assistant reasoning_content: %w", err)
				}
				out = next
				patchedReasoning++
			}

			for _, tc := range toolCalls.Array() {
				id := strings.TrimSpace(tc.Get("id").String())
				if id == "" {
					continue
				}
				pending = append(pending, id)
			}
		case "tool":
			toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())
			if toolCallID == "" {
				toolCallID = strings.TrimSpace(msg.Get("call_id").String())
				if toolCallID != "" {
					path := fmt.Sprintf("messages.%d.tool_call_id", msgIdx)
					next, err := sjson.SetBytes(out, path, toolCallID)
					if err != nil {
						return body, fmt.Errorf("kimi executor: failed to set tool_call_id from call_id: %w", err)
					}
					out = next
					patched++
				}
			}
			if toolCallID == "" {
				if len(pending) == 1 {
					toolCallID = pending[0]
					path := fmt.Sprintf("messages.%d.tool_call_id", msgIdx)
					next, err := sjson.SetBytes(out, path, toolCallID)
					if err != nil {
						return body, fmt.Errorf("kimi executor: failed to infer tool_call_id: %w", err)
					}
					out = next
					patched++
				} else if len(pending) > 1 {
					ambiguous++
				}
			}
			if toolCallID != "" {
				removePending(toolCallID)
			}
		}
	}

	if patched > 0 || patchedReasoning > 0 {
		log.WithFields(log.Fields{
			"patched_tool_messages":      patched,
			"patched_reasoning_messages": patchedReasoning,
		}).Debug("kimi executor: normalized tool message fields")
	}
	if ambiguous > 0 {
		log.WithFields(log.Fields{
			"ambiguous_tool_messages": ambiguous,
			"pending_tool_calls":      len(pending),
		}).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
	}

	return out, nil
}

func fallbackAssistantReasoning(msg gjson.Result, hasLatest bool, latest string) string {
	if hasLatest && strings.TrimSpace(latest) != "" {
		return latest
	}

	content := msg.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			return text
		}
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return "[reasoning unavailable]"
}

// Refresh refreshes the Kimi token using the refresh token.
func (e *KimiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("kimi executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("kimi executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	client := kimiauth.NewDeviceFlowClientWithDeviceID(e.cfg, resolveKimiDeviceID(auth))
	td, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ExpiresAt > 0 {
		exp := time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expired"] = exp
	}
	auth.Metadata["type"] = "kimi"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// applyKimiHeaders sets required headers for Kimi API requests.
// Headers match kimi-cli client for compatibility. Uses config values when available.
func applyKimiHeaders(r *http.Request, token string, stream bool, cfg *config.Config) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	// Use config values or defaults
	userAgent := "KimiCLI/1.10.6"
	platform := "kimi_cli"
	version := "1.10.6"

	if cfg != nil {
		if cfg.KimiHeaderDefaults.UserAgent != "" {
			userAgent = cfg.KimiHeaderDefaults.UserAgent
		}
		if cfg.KimiHeaderDefaults.Platform != "" {
			platform = cfg.KimiHeaderDefaults.Platform
		}
		if cfg.KimiHeaderDefaults.Version != "" {
			version = cfg.KimiHeaderDefaults.Version
		}
	}

	r.Header.Set("User-Agent", userAgent)
	r.Header.Set("X-Msh-Platform", platform)
	r.Header.Set("X-Msh-Version", version)
	r.Header.Set("X-Msh-Device-Name", getKimiHostname())
	r.Header.Set("X-Msh-Device-Model", getKimiDeviceModel())
	r.Header.Set("X-Msh-Device-Id", getKimiDeviceID())
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func resolveKimiDeviceIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	deviceIDRaw, ok := auth.Metadata["device_id"]
	if !ok {
		return ""
	}

	deviceID, ok := deviceIDRaw.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(deviceID)
}

func resolveKimiDeviceIDFromStorage(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}

	storage, ok := auth.Storage.(*kimiauth.KimiTokenStorage)
	if !ok || storage == nil {
		return ""
	}

	return strings.TrimSpace(storage.DeviceID)
}

func resolveKimiDeviceID(auth *cliproxyauth.Auth) string {
	deviceID := resolveKimiDeviceIDFromAuth(auth)
	if deviceID != "" {
		return deviceID
	}
	return resolveKimiDeviceIDFromStorage(auth)
}

func applyKimiHeadersWithAuth(r *http.Request, token string, stream bool, auth *cliproxyauth.Auth, cfg *config.Config) {
	applyKimiHeaders(r, token, stream, cfg)

	if deviceID := resolveKimiDeviceID(auth); deviceID != "" {
		r.Header.Set("X-Msh-Device-Id", deviceID)
	}
}

// getKimiHostname returns the machine hostname.
func getKimiHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// getKimiDeviceModel returns a device model string matching kimi-cli format.
func getKimiDeviceModel() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

// getKimiDeviceID returns a stable device ID, matching kimi-cli storage location.
func getKimiDeviceID() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "cli-proxy-api-device"
	}
	// Check kimi-cli's device_id location first (platform-specific)
	var kimiShareDir string
	switch runtime.GOOS {
	case "darwin":
		kimiShareDir = filepath.Join(homeDir, "Library", "Application Support", "kimi")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(homeDir, "AppData", "Roaming")
		}
		kimiShareDir = filepath.Join(appData, "kimi")
	default: // linux and other unix-like
		kimiShareDir = filepath.Join(homeDir, ".local", "share", "kimi")
	}
	deviceIDPath := filepath.Join(kimiShareDir, "device_id")
	if data, err := os.ReadFile(deviceIDPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "cli-proxy-api-device"
}

// kimiCreds extracts the access token from auth.
func kimiCreds(a *cliproxyauth.Auth) (token string) {
	if a == nil {
		return ""
	}
	// Check metadata first (OAuth flow stores tokens here)
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	// Fallback to attributes (API key style)
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			return v
		}
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	return ""
}

// stripKimiPrefix removes the "kimi-" prefix from model names for the upstream API.
func stripKimiPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "kimi-") {
		return model[5:]
	}
	return model
}

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

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
		resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/sentinel/chat-requirements"), headers, body) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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
	resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/conversation/init"), headers, body) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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
	resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/f/conversation/prepare"), prepareHeaders, body) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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
		resp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/files"), headers, payload) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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
		putResp, err := client.Do(putReq) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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
		doneResp, err := doCodexImageJSON(ctx, client, http.MethodPost, codexImageURL("/backend-api/files/"+created.FileID+"/uploaded"), headers, donePayload) //nolint:bodyclose // readAndCloseCodexImageBody closes the body below.
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

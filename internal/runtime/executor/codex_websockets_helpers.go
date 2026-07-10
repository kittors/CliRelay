package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// Keep aligned with upstream CLIProxyAPI (codex-tui).
	codexResponsesWebsocketBetaHeaderValue = "responses_websockets=2026-02-06"
)

func writeCodexWebsocketMessage(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) error {
	if sess != nil {
		return sess.writeMessage(conn, websocket.TextMessage, payload)
	}
	if conn == nil {
		return fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func buildCodexWebsocketRequestBody(body []byte, allowAppend bool) []byte {
	if len(body) == 0 {
		return nil
	}

	// Codex CLI websocket v2 uses `response.create` with `previous_response_id` for incremental turns.
	// The upstream ChatGPT Codex websocket currently rejects that with close 1008 (policy violation).
	// Fall back to v1 `response.append` semantics on the same websocket connection to keep the session alive.
	//
	// NOTE: The upstream expects the first websocket event on each connection to be `response.create`,
	// so we only use `response.append` after we have initialized the current connection.
	if allowAppend {
		if prev := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); prev != "" {
			inputNode := gjson.GetBytes(body, "input")
			wsReqBody := []byte(`{}`)
			wsReqBody, _ = sjson.SetBytes(wsReqBody, "type", "response.append")
			if inputNode.Exists() && inputNode.IsArray() && strings.TrimSpace(inputNode.Raw) != "" {
				wsReqBody, _ = sjson.SetRawBytes(wsReqBody, "input", []byte(inputNode.Raw))
				return wsReqBody
			}
			wsReqBody, _ = sjson.SetRawBytes(wsReqBody, "input", []byte("[]"))
			return wsReqBody
		}
	}

	wsReqBody, errSet := sjson.SetBytes(bytes.Clone(body), "type", "response.create")
	if errSet == nil && len(wsReqBody) > 0 {
		return wsReqBody
	}
	fallback := bytes.Clone(body)
	fallback, _ = sjson.SetBytes(fallback, "type", "response.create")
	return fallback
}

func readCodexWebsocketMessage(ctx context.Context, sess *codexWebsocketSession, conn *websocket.Conn, readCh chan codexWebsocketRead) (int, []byte, error) {
	if sess == nil {
		if conn == nil {
			return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
		}
		_ = conn.SetReadDeadline(time.Now().Add(codexResponsesWebsocketIdleTimeout))
		msgType, payload, errRead := conn.ReadMessage()
		return msgType, payload, errRead
	}
	if conn == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: websocket conn is nil")
	}
	if readCh == nil {
		return 0, nil, fmt.Errorf("codex websockets executor: session read channel is nil")
	}
	for {
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case ev, ok := <-readCh:
			if !ok {
				return 0, nil, fmt.Errorf("codex websockets executor: session read channel closed")
			}
			if ev.conn != conn {
				continue
			}
			if ev.err != nil {
				return 0, nil, ev.err
			}
			return ev.msgType, ev.payload, nil
		}
	}
}

func markCodexWebsocketCreateSent(sess *codexWebsocketSession, conn *websocket.Conn, payload []byte) {
	if sess == nil || conn == nil || len(payload) == 0 {
		return
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "response.create" {
		return
	}

	sess.connMu.Lock()
	if sess.conn == conn {
		sess.connCreateSent = true
	}
	sess.connMu.Unlock()
}

func buildCodexResponsesWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}
	return parsed.String(), nil
}

func applyCodexPromptCacheHeaders(auth *cliproxyauth.Auth, from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers
	}

	var cache codexCache
	if from == "claude" {
		userIDResult := gjson.GetBytes(req.Payload, "metadata.user_id")
		if userIDResult.Exists() {
			key := codexPromptCacheMapKey(auth, req.Model, userIDResult.String())
			if cached, ok := getCodexCache(key); ok {
				cache = cached
			} else {
				cache = codexCache{
					ID:     uuid.New().String(),
					Expire: time.Now().Add(1 * time.Hour),
				}
				setCodexCache(key, cache)
			}
		}
	} else if from == "openai-response" {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = codexAccountScopedExplicitSessionID(auth, promptCacheKey.String())
		}
	}

	if cache.ID != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "prompt_cache_key", cache.ID)
		headers.Set("Conversation_id", cache.ID)
		headers.Set("Session_id", cache.ID)
	}

	return rawJSON, headers
}

func applyCodexWebsocketHeaders(ctx context.Context, headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth, token string) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	var ginHeaders http.Header
	if ginCtx := ginContextFrom(ctx); ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	fp, fingerprintEnabled := codexIdentityFingerprint(cfg, auth, ctx)
	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(headers, fp, true)
	} else {
		misc.EnsureHeader(headers, ginHeaders, "x-codex-beta-features", "")
		misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
		misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-metadata", "")
		misc.EnsureHeader(headers, ginHeaders, "x-client-request-id", "")
		misc.EnsureHeader(headers, ginHeaders, "x-responsesapi-include-timing-metrics", "")

		// Align with upstream: Version is only propagated from client when present.
		misc.EnsureHeader(headers, ginHeaders, "Version", "")
		betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
		if betaHeader == "" && ginHeaders != nil {
			betaHeader = strings.TrimSpace(ginHeaders.Get("OpenAI-Beta"))
		}
		if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
			betaHeader = codexResponsesWebsocketBetaHeaderValue
		}
		headers.Set("OpenAI-Beta", betaHeader)
		misc.EnsureHeader(headers, ginHeaders, "User-Agent", codexUserAgent)
	}

	// Match upstream: only attach Session_id when UA indicates a desktop client, and do not forward UA over websocket.
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && strings.TrimSpace(headers.Get("Session_id")) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
	headers.Del("User-Agent")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}

	originatorFromClient := ""
	if ginHeaders != nil {
		originatorFromClient = strings.TrimSpace(ginHeaders.Get("Originator"))
	}
	if originatorFromClient != "" {
		headers.Set("Originator", originatorFromClient)
	} else if !isAPIKey {
		if fingerprintEnabled {
			headers.Set("Originator", fp.Originator)
		} else {
			headers.Set("Originator", codexOriginator)
		}
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					headers.Set("Chatgpt-Account-Id", trimmed)
				}
			}
		}
	}

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(headers, fp, true)
		if !isAPIKey {
			if originator := strings.TrimSpace(fp.Originator); originator != "" {
				headers.Set("Originator", originator)
			} else {
				headers.Del("Originator")
			}
		}
	}
	// Ensure UA remains absent even if custom headers attempted to set it.
	headers.Del("User-Agent")

	return headers
}

type statusErrWithHeaders struct {
	statusErr
	headers http.Header
}

func (e statusErrWithHeaders) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

func parseCodexWebsocketError(payload []byte) (error, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "error" {
		return nil, false
	}
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		return nil, false
	}

	out := []byte(`{}`)
	if errNode := gjson.GetBytes(payload, "error"); errNode.Exists() {
		raw := errNode.Raw
		if errNode.Type == gjson.String {
			raw = errNode.Raw
		}
		out, _ = sjson.SetRawBytes(out, "error", []byte(raw))
	} else {
		out, _ = sjson.SetBytes(out, "error.type", "server_error")
		out, _ = sjson.SetBytes(out, "error.message", http.StatusText(status))
	}

	headers := parseCodexWebsocketErrorHeaders(payload)
	return statusErrWithHeaders{
		statusErr: statusErr{code: status, msg: string(out)},
		headers:   headers,
	}, true
}

func parseCodexWebsocketErrorHeaders(payload []byte) http.Header {
	headersNode := gjson.GetBytes(payload, "headers")
	if !headersNode.Exists() || !headersNode.IsObject() {
		return nil
	}
	mapped := make(http.Header)
	headersNode.ForEach(func(key, value gjson.Result) bool {
		name := strings.TrimSpace(key.String())
		if name == "" {
			return true
		}
		switch value.Type {
		case gjson.String:
			if v := strings.TrimSpace(value.String()); v != "" {
				mapped.Set(name, v)
			}
		case gjson.Number, gjson.True, gjson.False:
			if v := strings.TrimSpace(value.Raw); v != "" {
				mapped.Set(name, v)
			}
		default:
		}
		return true
	})
	if len(mapped) == 0 {
		return nil
	}
	return mapped
}

func normalizeCodexWebsocketCompletion(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

func encodeCodexWebsocketAsSSE(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	line := make([]byte, 0, len("data: ")+len(payload))
	line = append(line, []byte("data: ")...)
	line = append(line, payload...)
	return line
}

func websocketHandshakeBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	body := readUpstreamErrorBody("codex-websocket", resp.Body)
	closeHTTPResponseBody(resp, "codex websockets executor: close handshake response body error")
	if len(body) == 0 {
		return nil
	}
	return body
}

func closeHTTPResponseBody(resp *http.Response, logPrefix string) {
	if resp == nil || resp.Body == nil {
		return
	}
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("%s: %v", logPrefix, errClose)
	}
}

func executionSessionIDFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func codexWebsocketsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	if len(auth.Attributes) > 0 {
		if raw := strings.TrimSpace(auth.Attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(v))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

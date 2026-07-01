// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements a Codex executor that uses the Responses API WebSocket transport.
package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesWebsocketIdleTimeout = 5 * time.Minute
	codexResponsesWebsocketHandshakeTO = 30 * time.Second
)

// CodexWebsocketsExecutor executes Codex Responses requests using a WebSocket transport.
//
// It preserves the existing CodexExecutor HTTP implementation as a fallback for endpoints
// not available over WebSocket (e.g. /responses/compact) and for websocket upgrade failures.
type CodexWebsocketsExecutor struct {
	*CodexExecutor

	sessMu   sync.Mutex
	sessions map[string]*codexWebsocketSession
}

type codexWebsocketSession struct {
	sessionID string

	reqMu sync.Mutex

	connMu sync.Mutex
	conn   *websocket.Conn
	wsURL  string
	authID string

	// connCreateSent tracks whether a `response.create` message has been successfully sent
	// on the current websocket connection. The upstream expects the first message on each
	// connection to be `response.create`.
	connCreateSent bool

	writeMu sync.Mutex

	activeMu     sync.Mutex
	activeCh     chan codexWebsocketRead
	activeDone   <-chan struct{}
	activeCancel context.CancelFunc

	readerConn *websocket.Conn
}

type codexWebsocketRead struct {
	conn    *websocket.Conn
	msgType int
	payload []byte
	err     error
}

func NewCodexWebsocketsExecutor(cfg *config.Config) *CodexWebsocketsExecutor {
	return &CodexWebsocketsExecutor{
		CodexExecutor: NewCodexExecutor(cfg),
		sessions:      make(map[string]*codexWebsocketSession),
	}
}

func (e *CodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return e.CodexExecutor.executeCompact(ctx, auth, req, opts)
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat: sdktranslator.FromString("codex"),
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return resp, errAdmission
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	to := execCtx.Execution.TargetFormat
	body, originalTranslated := execCtx.TranslateRequestPair(req.Payload)

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	body = execCtx.ApplyPayloadConfig(body, originalTranslated)
	body, _ = sjson.SetBytes(body, "model", execCtx.BaseModel)
	body = sanitizeCodexResponsesRequest(body)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return resp, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(auth, execCtx.SourceFormat, req, body)
	wsHeaders = applyCodexWebsocketHeaders(execCtx.Context, wsHeaders, e.cfg, auth, apiKey)
	recorder := execCtx.Recorder()

	var authID string
	if auth != nil {
		authID = auth.ID
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		sess.reqMu.Lock()
		defer sess.reqMu.Unlock()
	}

	allowAppend := true
	if sess != nil {
		sess.connMu.Lock()
		allowAppend = sess.connCreateSent
		sess.connMu.Unlock()
	}
	wsReqBody := buildCodexWebsocketRequestBody(body, allowAppend)
	recorder.RecordRequest(wsURL, "WEBSOCKET", wsHeaders.Clone(), wsReqBody)

	conn, respHS, errDial := e.ensureUpstreamConn(execCtx.Context, auth, sess, authID, wsURL, wsHeaders)
	if respHS != nil {
		recorder.RecordResponseMetadata(respHS.StatusCode, respHS.Header.Clone())
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if len(bodyErr) > 0 {
			recorder.AppendResponseChunk(bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			return e.CodexExecutor.Execute(execCtx.Context, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return resp, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		recorder.RecordResponseError(errDial)
		return resp, errDial
	}
	closeHTTPResponseBody(respHS, "codex websockets executor: close handshake response body error")
	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
		defer func() {
			reason := "completed"
			if err != nil {
				reason = "error"
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, reason, err)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
		defer sess.clearActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a fresh websocket connection. This is mainly to handle
			// upstream closing the socket between sequential requests within the same
			// execution session.
			connRetry, respRetry, errDialRetry := e.ensureUpstreamConn(execCtx.Context, auth, sess, authID, wsURL, wsHeaders) //nolint:bodyclose // failed handshake body is closed below; successful websocket response is owned by connRetry.
			if errDialRetry != nil {
				_ = websocketHandshakeBody(respRetry)
			}
			if errDialRetry == nil && connRetry != nil {
				sess.connMu.Lock()
				allowAppend = sess.connCreateSent
				sess.connMu.Unlock()
				wsReqBodyRetry := buildCodexWebsocketRequestBody(body, allowAppend)
				recorder.RecordRequest(wsURL, "WEBSOCKET", wsHeaders.Clone(), wsReqBodyRetry)
				if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry == nil {
					conn = connRetry
					wsReqBody = wsReqBodyRetry
				} else {
					e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
					recorder.RecordResponseError(errSendRetry)
					return resp, errSendRetry
				}
			} else {
				recorder.RecordResponseError(errDialRetry)
				return resp, errDialRetry
			}
		} else {
			recorder.RecordResponseError(errSend)
			return resp, errSend
		}
	}
	markCodexWebsocketCreateSent(sess, conn, wsReqBody)

	for {
		if execCtx.Context != nil && execCtx.Context.Err() != nil {
			return resp, execCtx.Context.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(execCtx.Context, sess, conn, readCh)
		if errRead != nil {
			recorder.RecordResponseError(errRead)
			return resp, errRead
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				err = fmt.Errorf("codex websockets executor: unexpected binary message")
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
				}
				recorder.RecordResponseError(err)
				return resp, err
			}
			continue
		}

		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		recorder.AppendResponseChunk(payload)

		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
			}
			recorder.RecordResponseError(wsErr)
			return resp, wsErr
		}

		payload = normalizeCodexWebsocketCompletion(payload)
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" {
			if detail, ok := parseCodexUsage(payload); ok {
				reporter.publish(execCtx.Context, detail)
			}
			var param any
			out := sdktranslator.TranslateNonStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, execCtx.OriginalPayload, body, payload, &param)
			resp = cliproxyexecutor.Response{Payload: []byte(out)}
			return resp, nil
		}
	}
}

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	authIDForLog := ""
	if auth != nil {
		authIDForLog = auth.ID
	}
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", authIDForLog, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	execCtx := newExecutionContext(ctx, e.Identifier(), e.cfg, auth, req, opts, ExecutionOptions{
		TargetFormat:      sdktranslator.FromString("codex"),
		TranslateAsStream: true,
	})
	reporter := execCtx.Reporter()
	defer reporter.trackFailure(execCtx.Context, &err)

	if errAdmission := enforceCodexClientAdmission(execCtx.Context, e.cfg, auth); errAdmission != nil {
		return nil, errAdmission
	}

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	to := execCtx.Execution.TargetFormat
	body := req.Payload

	body, err = thinking.ApplyThinking(body, req.Model, execCtx.SourceFormat.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	body = execCtx.ApplyPayloadConfig(body, body)

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders := applyCodexPromptCacheHeaders(auth, execCtx.SourceFormat, req, body)
	wsHeaders = applyCodexWebsocketHeaders(execCtx.Context, wsHeaders, e.cfg, auth, apiKey)
	recorder := execCtx.Recorder()

	var authID string
	if auth != nil {
		authID = auth.ID
	}

	executionSessionID := executionSessionIDFromOptions(opts)
	var sess *codexWebsocketSession
	if executionSessionID != "" {
		sess = e.getOrCreateSession(executionSessionID)
		sess.reqMu.Lock()
	}

	allowAppend := true
	if sess != nil {
		sess.connMu.Lock()
		allowAppend = sess.connCreateSent
		sess.connMu.Unlock()
	}
	wsReqBody := buildCodexWebsocketRequestBody(body, allowAppend)
	recorder.RecordRequest(wsURL, "WEBSOCKET", wsHeaders.Clone(), wsReqBody)

	conn, respHS, errDial := e.ensureUpstreamConn(execCtx.Context, auth, sess, authID, wsURL, wsHeaders)
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
		recorder.RecordResponseMetadata(respHS.StatusCode, respHS.Header.Clone())
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if len(bodyErr) > 0 {
			recorder.AppendResponseChunk(bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			return e.CodexExecutor.ExecuteStream(execCtx.Context, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			return nil, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		recorder.RecordResponseError(errDial)
		if sess != nil {
			sess.reqMu.Unlock()
		}
		return nil, errDial
	}
	closeHTTPResponseBody(respHS, "codex websockets executor: close handshake response body error")

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = make(chan codexWebsocketRead, 4096)
		sess.setActive(readCh)
	}

	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		recorder.RecordResponseError(errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)

			// Retry once with a new websocket connection for the same execution session.
			connRetry, respRetry, errDialRetry := e.ensureUpstreamConn(execCtx.Context, auth, sess, authID, wsURL, wsHeaders) //nolint:bodyclose // failed handshake body is closed below; successful websocket response is owned by connRetry.
			if errDialRetry != nil {
				_ = websocketHandshakeBody(respRetry)
			}
			if errDialRetry != nil || connRetry == nil {
				recorder.RecordResponseError(errDialRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errDialRetry
			}
			sess.connMu.Lock()
			allowAppend = sess.connCreateSent
			sess.connMu.Unlock()
			wsReqBodyRetry := buildCodexWebsocketRequestBody(body, allowAppend)
			recorder.RecordRequest(wsURL, "WEBSOCKET", wsHeaders.Clone(), wsReqBodyRetry)
			if errSendRetry := writeCodexWebsocketMessage(sess, connRetry, wsReqBodyRetry); errSendRetry != nil {
				recorder.RecordResponseError(errSendRetry)
				e.invalidateUpstreamConn(sess, connRetry, "send_error", errSendRetry)
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return nil, errSendRetry
			}
			conn = connRetry
			wsReqBody = wsReqBodyRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}
	markCodexWebsocketCreateSent(sess, conn, wsReqBody)

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error

		defer close(out)
		defer func() {
			if sess != nil {
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if execCtx.Context == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-execCtx.Context.Done():
				return false
			}
		}

		var param any
		for {
			if execCtx.Context != nil && execCtx.Context.Err() != nil {
				terminateReason = "context_done"
				terminateErr = execCtx.Context.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: execCtx.Context.Err()})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(execCtx.Context, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && execCtx.Context != nil && execCtx.Context.Err() != nil {
					terminateReason = "context_done"
					terminateErr = execCtx.Context.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: execCtx.Context.Err()})
					return
				}
				terminateReason = "read_error"
				terminateErr = errRead
				recorder.RecordResponseError(errRead)
				reporter.publishFailure(execCtx.Context)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					err = fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = err
					recorder.RecordResponseError(err)
					reporter.publishFailure(execCtx.Context)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: err})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			recorder.AppendResponseChunk(payload)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = wsErr
				recorder.RecordResponseError(wsErr)
				reporter.publishFailure(execCtx.Context)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			payload = normalizeCodexWebsocketCompletion(payload)
			eventType := gjson.GetBytes(payload, "type").String()
			if eventType == "response.completed" || eventType == "response.done" {
				if detail, ok := parseCodexUsage(payload); ok {
					reporter.publish(execCtx.Context, detail)
				}
			}

			line := encodeCodexWebsocketAsSSE(payload)
			chunks := sdktranslator.TranslateStream(execCtx.Context, to, execCtx.SourceFormat, req.Model, body, body, line, &param)
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}) {
					terminateReason = "context_done"
					terminateErr = execCtx.Context.Err()
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" {
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}

func (e *CodexWebsocketsExecutor) dialCodexWebsocket(ctx context.Context, auth *cliproxyauth.Auth, wsURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := newProxyAwareWebsocketDialer(ctx, e.cfg, auth)
	dialer.HandshakeTimeout = codexResponsesWebsocketHandshakeTO
	dialer.EnableCompression = true
	if ctx == nil {
		ctx = context.Background()
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if conn != nil {
		// Avoid gorilla/websocket flate tail validation issues on some upstreams/Go versions.
		// Negotiating permessage-deflate is fine; we just don't compress outbound messages.
		conn.EnableWriteCompression(false)
	}
	return conn, resp, err
}

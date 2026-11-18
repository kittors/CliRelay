package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Incremental Codex requests: the websocket transport.
//
// An "on" turn goes out on its chain's websocket: as an increment when it
// extends the base that connection holds, in full otherwise. What comes back is
// served to the executor as the SSE body an HTTP request would have had, so
// translation, usage, the request log and error handling run unchanged.
//
// Nothing here may cost the client its turn. A turn the websocket cannot carry
// goes over HTTP; a continuation the backend no longer knows goes again in full;
// only a refusal of the request itself (429, 401, a bad request) is passed on,
// as the HTTP status it stands for, because HTTP would draw the same answer.

// doCodexResponsesIncremental sends a prepared Responses call, over the turn's
// websocket when it has one and over HTTP otherwise.
func (e *CodexExecutor) doCodexResponsesIncremental(execCtx *ExecutionContext, recorder UpstreamRecorder, httpReq *http.Request, fallbackBody []byte, turn *codexIncrementalTurn, token string) (*http.Response, error) {
	if resp, handled, err := e.sendCodexIncremental(execCtx, recorder, httpReq, turn, token); handled {
		return resp, err
	}
	return e.doCodexResponses(execCtx, recorder, httpReq, fallbackBody)
}

// sendCodexIncremental sends an "on" turn over the websocket. handled is false
// when the turn must go over HTTP; the record says why.
func (e *CodexExecutor) sendCodexIncremental(execCtx *ExecutionContext, recorder UpstreamRecorder, httpReq *http.Request, t *codexIncrementalTurn, token string) (*http.Response, bool, error) {
	if t == nil || t.mode != config.CodexUpstreamIncrementalOn || t.chain == nil || t.cur == nil || httpReq == nil {
		return nil, false, nil
	}
	defer t.cur.dropContent()
	ctx := httpReq.Context()
	full := !t.hit
	forceNew := false
	for attempt := 0; attempt < 2; attempt++ {
		if full && len(t.cur.body) > t.maxFirst {
			// Checked before dialling: a socket opened for a request that will
			// not use it is a connection the account carries for nothing.
			return t.fallback("first_request_too_large")
		}
		conn, reused, reason := e.codexIncrementalConnection(execCtx, httpReq, t, token, forceNew)
		if reason != "" {
			return t.fallback(reason)
		}
		if t.chain.base == nil || conn.serial != t.chain.base.connSerial {
			// Only the connection that produced the base can continue it.
			full = true
		}
		var msg []byte
		if full {
			if len(t.cur.body) > t.maxFirst {
				return t.fallback("first_request_too_large")
			}
			msg = buildCodexFullCreate(t.cur)
		} else {
			msg = buildCodexIncrementalCreate(t.cur, t.match.newItems, t.chain.base.responseID)
		}
		sink, errBegin := conn.beginTurn()
		if errBegin != nil {
			forceNew, full = true, true
			continue
		}
		recorder.RecordRequest(conn.wsURL, "WEBSOCKET", conn.handshake.Clone(), msg)
		t.wsSent += len(msg)
		if errSend := conn.send(ctx, msg); errSend != nil {
			conn.endTurn(sink)
			if ctx.Err() != nil {
				t.pool.dropConn(t.chain)
				return nil, true, ctx.Err()
			}
			if reused && attempt == 0 {
				// The backend closed the idle socket under us; a new one gets
				// the whole request.
				t.retried("send_failed")
				forceNew, full = true, true
				continue
			}
			return t.fallback("send_failed")
		}
		preamble, first, errRead := readCodexIncrementalFirstEvent(ctx, sink, conn)
		if errRead != nil {
			conn.endTurn(sink)
			if ctx.Err() != nil {
				// The request was sent; its events would reach the next turn.
				t.pool.dropConn(t.chain)
				return nil, true, ctx.Err()
			}
			if attempt == 0 {
				t.retried("connection_lost")
				forceNew, full = true, true
				continue
			}
			return t.fallback("connection_lost")
		}
		if gjson.GetBytes(first, "type").String() == "error" {
			conn.endTurn(sink)
			kind := classifyCodexIncrementalError(first)
			switch {
			case attempt == 0 && kind == "previous_response_not_found" && !full:
				// The backend dropped the base (a 4xx or 5xx on the lane drops
				// it); the connection itself is fine.
				t.retried(kind)
				full = true
				continue
			case attempt == 0 && kind == "connection_limit_reached":
				t.retried(kind)
				forceNew, full = true, true
				continue
			case kind == "invalid_encrypted_content":
				// The HTTP path answers this by sending the turn again without
				// the replayed reasoning (codex_responses_send.go); hand it the
				// turn rather than repeat that here.
				return t.fallback(kind)
			}
			t.pool.dropConn(t.chain)
			t.record.update(func(r *codexIncrementalRecord) {
				r.Transport, r.SentBytes, r.ConnReused, r.ConnSerial = "websocket", t.wsSent, reused, conn.serial
			})
			return codexIncrementalErrorResponse(first), true, nil
		}
		t.conn, t.sink, t.sentIncrement = conn, sink, !full
		t.record.update(func(r *codexIncrementalRecord) {
			r.Transport, r.SentBytes, r.ConnReused, r.ConnSerial = "websocket", t.wsSent, reused, conn.serial
			if full && r.Result == "hit" {
				// It matched, but went in full (a retry or a new connection).
				r.Result = "miss"
				if r.Reason == "" {
					r.Reason = "sent_in_full"
				}
			}
		})
		body := &codexIncrementalBody{ctx: ctx, turn: t, pending: append(preamble, first), chatGPT: isCodexChatGPTBackend(execCtx.Auth, httpReq.URL)}
		header := http.Header{}
		header.Set("Content-Type", "text/event-stream")
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: body, Request: httpReq}, true, nil
	}
	return t.fallback("retry_exhausted")
}

// codexIncrementalConnection returns the chain's connection when it can take
// this turn, or opens one.
func (e *CodexExecutor) codexIncrementalConnection(execCtx *ExecutionContext, httpReq *http.Request, t *codexIncrementalTurn, token string, forceNew bool) (*codexIncrementalConn, bool, string) {
	ctx := httpReq.Context()
	auth := execCtx.Auth
	if !forceNew && t.chain.conn.usable(t.identity, time.Now()) {
		// No dial this time, so the request log learns the egress here.
		recordRequestLogEgressRoute(ctx, e.cfg, auth, t.chain.conn.proxyURL)
		return t.chain.conn, true, ""
	}
	if !t.pool.reserveConn(t.chain, t.limits) {
		return nil, false, "connection_limit"
	}
	wsURL, err := buildCodexResponsesWebsocketURL(httpReq.URL.String())
	if err != nil {
		t.pool.releaseReservation(t.chain)
		return nil, false, "websocket_url"
	}
	cacheID := ""
	switch t.from {
	case sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI:
		// For these the cache helper set prompt_cache_key; see cacheRequest.
		cacheID = t.cur.promptCacheKey
	}
	headers := codexPromptCacheSessionHeaders(e.cfg, t.from, cacheID)
	headers = applyCodexWebsocketHeaders(ctx, headers, e.cfg, auth, token)
	applyCodexServiceTierRoutingHint(headers, t.cur.body, auth)
	conn, handshake, errDial := dialCodexIncrementalConn(ctx, e.cfg, auth, wsURL, t.identity, headers)
	observeCodexWebsocketHandshake(ctx, isCodexChatGPTBackend(auth, httpReq.URL), handshake)
	status := 0
	if handshake != nil {
		status = handshake.StatusCode
	}
	if errDial != nil {
		_ = websocketHandshakeBody(handshake)
		t.pool.releaseReservation(t.chain)
		logWithRequestID(ctx).Infof("codex incremental: upstream websocket not opened (status=%d): %v; sending over HTTP", status, errDial)
		if status > 0 {
			return nil, false, fmt.Sprintf("handshake_%d", status)
		}
		return nil, false, "dial_failed"
	}
	closeHTTPResponseBody(handshake, "codex incremental: close handshake response body error")
	conn.handshake = headers
	t.pool.attachConn(t.chain, conn)
	return conn, false, ""
}

func (t *codexIncrementalTurn) fallback(reason string) (*http.Response, bool, error) {
	t.record.update(func(r *codexIncrementalRecord) {
		r.Fallback = reason
		if r.Result == "hit" {
			r.Result = "miss"
			r.Reason = "fallback"
		}
		// The HTTP request carries the whole body, after whatever the socket
		// already took.
		r.SentBytes = t.wsSent + r.FullBytes
	})
	return nil, false, nil
}

func (t *codexIncrementalTurn) retried(reason string) {
	t.record.update(func(r *codexIncrementalRecord) { r.Retried = reason })
}

// dropConn closes the chain's connection after the backend refused a turn on it.
func (p *codexIncrementalPool) dropConn(chain *codexIncrementalChain) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgetConnLocked(chain)
}

// readCodexIncrementalFirstEvent waits for the first response event or error,
// keeping what came before it (rate-limit snapshots) to pass on in order.
func readCodexIncrementalFirstEvent(ctx context.Context, sink *codexIncrementalTurnSink, conn *codexIncrementalConn) ([][]byte, []byte, error) {
	var preamble [][]byte
	for {
		payload, err := sink.next(ctx, conn.done)
		if err != nil {
			return nil, nil, err
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == "error" || strings.HasPrefix(eventType, "response.") {
			return preamble, payload, nil
		}
		if len(preamble) >= 32 {
			return nil, nil, errors.New("codex incremental: no response event among the first messages")
		}
		preamble = append(preamble, payload)
	}
}

// classifyCodexIncrementalError names the websocket errors the transport
// recovers from. The backend answered a stale id on a new connection with
// "Invalid previous_response_id" (2026-11-18); the documented code is
// previous_response_not_found. The 60-minute limit is
// websocket_connection_limit_reached.
func classifyCodexIncrementalError(payload []byte) string {
	code := strings.ToLower(codexResponsesFailedField(payload, "error.code", "code"))
	message := strings.ToLower(codexResponsesFailedField(payload, "error.message", "message"))
	switch {
	case code == "previous_response_not_found",
		strings.Contains(message, "previous_response"),
		strings.Contains(message, "previous response") && strings.Contains(message, "not found"):
		return "previous_response_not_found"
	case code == "websocket_connection_limit_reached",
		strings.Contains(message, "connection limit") && strings.Contains(message, "websocket"):
		return "connection_limit_reached"
	case bytes.Contains(payload, codexInvalidCiphertextCode):
		return "invalid_encrypted_content"
	}
	return ""
}

// codexIncrementalErrorResponse turns a websocket error event into the HTTP
// answer it stands for, so cooling, quota and retry handling see a 429 as a 429.
func codexIncrementalErrorResponse(payload []byte) *http.Response {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status < http.StatusBadRequest {
		status = codexResponsesFailedHTTPStatus(codexResponsesFailedField(payload, "error.code", "code"), codexResponsesFailedField(payload, "error.type"))
	}
	body := []byte(`{}`)
	if errNode := gjson.GetBytes(payload, "error"); errNode.Exists() {
		body, _ = sjson.SetRawBytes(body, "error", []byte(errNode.Raw))
	} else {
		body, _ = sjson.SetBytes(body, "error.type", "server_error")
		body, _ = sjson.SetBytes(body, "error.message", http.StatusText(status))
	}
	header := parseCodexWebsocketErrorHeaders(payload)
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

// codexIncrementalBody serves one websocket turn as an SSE body. It ends after
// the turn's final event, leaving the connection to the chain.
type codexIncrementalBody struct {
	ctx       context.Context
	turn      *codexIncrementalTurn
	pending   [][]byte
	buf       bytes.Buffer
	done      bool
	err       error
	chatGPT   bool
	closeOnce sync.Once
}

func (b *codexIncrementalBody) Read(p []byte) (int, error) {
	for b.buf.Len() == 0 {
		if b.done {
			return 0, b.err
		}
		var payload []byte
		if len(b.pending) > 0 {
			payload, b.pending = b.pending[0], b.pending[1:]
		} else {
			next, err := b.turn.sink.next(b.ctx, b.turn.conn.done)
			if err != nil {
				b.done, b.err = true, err
				continue
			}
			payload = next
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		payload = normalizeCodexWebsocketCompletion(payload)
		observeCodexWebsocketMessage(b.ctx, b.chatGPT, payload)
		eventType := gjson.GetBytes(payload, "type").String()
		writeCodexIncrementalSSE(&b.buf, eventType, payload)
		switch eventType {
		case "response.completed", "response.incomplete", "response.failed", "error":
			b.turn.terminal = true
			b.done, b.err = true, io.EOF
		}
	}
	return b.buf.Read(p)
}

func (b *codexIncrementalBody) Close() error {
	b.closeOnce.Do(func() {
		if b.turn.conn != nil {
			b.turn.conn.endTurn(b.turn.sink)
		}
		if !b.turn.terminal {
			log.Debugf("codex incremental: turn on conn=%d closed before its final event", b.turn.conn.serial)
		}
	})
	return nil
}

// writeCodexIncrementalSSE frames one event the way the HTTP transport delivers
// it, splitting a payload that spans lines into data fields.
func writeCodexIncrementalSSE(buf *bytes.Buffer, eventType string, payload []byte) {
	if eventType != "" && !strings.ContainsAny(eventType, "\r\n") {
		buf.WriteString("event: ")
		buf.WriteString(eventType)
		buf.WriteByte('\n')
	}
	for {
		line, rest, more := bytes.Cut(payload, []byte{'\n'})
		buf.WriteString("data: ")
		buf.Write(bytes.TrimSuffix(line, []byte{'\r'}))
		buf.WriteByte('\n')
		if !more {
			break
		}
		payload = rest
	}
	buf.WriteByte('\n')
}

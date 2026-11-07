package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// wsErrorCodePreviousResponseNotFound is the code Codex answers by sending the
	// request again in full: codex-rs 0.166 maps it to ApiError::Retryable in
	// map_wrapped_websocket_error_event (codex-api/src/endpoint/responses_websocket.rs).
	wsErrorCodePreviousResponseNotFound = "previous_response_not_found"

	// wsSessionLogMaxBytes bounds the event log a connection keeps for the request
	// log. One connection lives for a whole Codex session, so without a bound the
	// log grows with every turn until the client leaves.
	wsSessionLogMaxBytes        = 8 << 20
	wsSessionLogTruncatedMarker = "websocket.truncated\n(later events omitted: the session log reached its size limit)\n"
)

// responsesWebsocketSettings is the part of the configuration one connection
// reads when it opens.
type responsesWebsocketSettings struct {
	enabled       bool
	compression   bool
	maxMessage    int64
	maxTranscript int64
	idleTimeout   time.Duration
}

func (h *OpenAIResponsesAPIHandler) websocketSettings() responsesWebsocketSettings {
	var cfg *sdkconfig.SDKConfig
	if h != nil && h.BaseAPIHandler != nil {
		cfg = h.Cfg
	}
	return responsesWebsocketSettings{
		enabled:       cfg.ResponsesWebsocketEnabled(),
		compression:   cfg.ResponsesWebsocketCompressionEnabled(),
		maxMessage:    cfg.ResponsesWebsocketMaxMessageBytes(),
		maxTranscript: cfg.ResponsesWebsocketMaxTranscriptBytes(),
		idleTimeout:   cfg.ResponsesWebsocketIdleTimeout(),
	}
}

// responsesWebsocketTranscript is what one connection keeps between turns to
// expand Codex's increments (previous_response_id plus the input items added
// since) into the complete request an HTTP upstream needs.
type responsesWebsocketTranscript struct {
	lastRequest        []byte
	lastResponseOutput []byte
	// lastResponseID is the response an increment may continue: the response.id
	// of the last turn's terminal event, or the synthetic prewarm id. It is empty
	// after a turn that ended without one.
	lastResponseID string
	// lastWindowID is Codex's local context window. Codex opens a new window when
	// it compacts the conversation.
	lastWindowID string
}

func newResponsesWebsocketTranscript() *responsesWebsocketTranscript {
	return &responsesWebsocketTranscript{lastResponseOutput: []byte("[]")}
}

// sizeBytes is how much the transcript holds in memory between turns.
func (t *responsesWebsocketTranscript) sizeBytes() int64 {
	return int64(len(t.lastRequest) + len(t.lastResponseOutput))
}

// wsTurn says how a request relates to the transcript the connection holds.
type wsTurn int

const (
	// wsTurnFollowUp goes to the existing normalizer: the first request on the
	// connection, a v1 response.append, or an increment naming the last response
	// (any increment, when the upstream keeps the context itself).
	wsTurnFollowUp wsTurn = iota
	// wsTurnNewRoot is a complete request that replaces the transcript.
	wsTurnNewRoot
	// wsTurnStaleRoot is a complete request that still names a response from an
	// earlier context window; that id is dropped before the request goes upstream.
	wsTurnStaleRoot
	// wsTurnUnknownPrevious names a response this connection cannot expand.
	wsTurnUnknownPrevious
)

// classify tells a complete request from an increment.
//
// In the v2 protocol Codex speaks, every request is a response.create and an
// increment always names the response it continues. A response.create without
// previous_response_id is therefore complete: Codex sends one whenever its reuse
// checks fail (the model, tools or reasoning changed, or the history no longer
// extends the previous one). Expanding it as an increment would send the
// conversation upstream twice.
//
// Two complete requests still carry an id. After Codex compacts into a new
// context window it may keep naming the previous window's last response (the
// window id changes), and a remote compaction replays the conversation with
// compaction items in it. Both start a new transcript.
func (t *responsesWebsocketTranscript) classify(payload []byte, upstreamKeepsContext bool) wsTurn {
	if len(t.lastRequest) == 0 || strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != wsRequestTypeCreate {
		return wsTurnFollowUp
	}
	previousID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
	if previousID == "" {
		return wsTurnNewRoot
	}
	if t.windowChanged(payload) || websocketInputHasCompaction(payload) {
		return wsTurnStaleRoot
	}
	if upstreamKeepsContext || previousID == t.lastResponseID {
		return wsTurnFollowUp
	}
	return wsTurnUnknownPrevious
}

func (t *responsesWebsocketTranscript) windowChanged(payload []byte) bool {
	window := websocketPayloadWindowID(payload)
	return window != "" && t.lastWindowID != "" && window != t.lastWindowID
}

// observeWindow remembers the context window of the request just accepted.
func (t *responsesWebsocketTranscript) observeWindow(payload []byte) {
	if window := websocketPayloadWindowID(payload); window != "" {
		t.lastWindowID = window
	}
}

// websocketPayloadWindowID reads Codex's local context window id where sub2api
// reads it: client_metadata["x-codex-window-id"], or the window_id inside
// client_metadata["x-codex-turn-metadata"].
func websocketPayloadWindowID(payload []byte) string {
	metadata := gjson.GetBytes(payload, "client_metadata")
	if !metadata.IsObject() {
		return ""
	}
	if window := strings.TrimSpace(metadata.Get("x-codex-window-id").String()); window != "" {
		return window
	}
	turnMetadata := strings.TrimSpace(metadata.Get("x-codex-turn-metadata").String())
	if turnMetadata == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(turnMetadata, "window_id").String())
}

// websocketInputHasCompaction reports whether the input replays a compacted
// conversation, which a client only ever sends in full.
func websocketInputHasCompaction(payload []byte) bool {
	for _, item := range gjson.GetBytes(payload, "input").Array() {
		switch item.Get("type").String() {
		case "compaction", "compaction_summary":
			return true
		}
	}
	return false
}

// dropPreviousResponseID removes a previous_response_id that no longer names the
// response the conversation continues.
func dropPreviousResponseID(payload []byte) []byte {
	out, err := sjson.DeleteBytes(payload, "previous_response_id")
	if err != nil {
		return payload
	}
	return out
}

// websocketResponseFinished reports whether eventType ends a turn with a reply:
// response.completed, or response.incomplete for a reply the upstream cut short
// (passed on as a result, as on the HTTP entries). response.done, the name older
// clients used, is still accepted from upstreams. It is never sent to clients:
// Codex 0.110 and later ignore it and wait for the idle timeout instead.
func websocketResponseFinished(eventType string) bool {
	switch eventType {
	case wsEventTypeCompleted, wsEventTypeIncomplete, wsEventTypeDone:
		return true
	}
	return false
}

// websocketOutputCollector keeps the items announced by response.output_item.done
// so the transcript gets every item of a reply even if the terminal event lists
// only some of them; the non-stream Codex path guards against the same thing
// (mergeCodexResponsesCompletedOutput).
type websocketOutputCollector struct {
	items [][]byte
	keys  []string
	seen  map[string]struct{}
}

func (c *websocketOutputCollector) observe(payload []byte) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(payload, "item")
	if !item.IsObject() {
		return
	}
	key := websocketOutputItemKey(item)
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, ok := c.seen[key]; ok {
		return
	}
	c.seen[key] = struct{}{}
	c.items = append(c.items, []byte(item.Raw))
	c.keys = append(c.keys, key)
}

// output returns the terminal event's response.output with any item announced
// earlier but missing from it appended.
func (c *websocketOutputCollector) output(terminal []byte) []byte {
	listed := gjson.GetBytes(terminal, "response.output")
	listedItems := listed.Array()
	merged := make([][]byte, 0, len(listedItems)+len(c.items))
	seen := make(map[string]struct{}, len(listedItems)+len(c.items))
	for _, item := range listedItems {
		key := websocketOutputItemKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, []byte(item.Raw))
	}
	appended := false
	for i, item := range c.items {
		if _, ok := seen[c.keys[i]]; ok {
			continue
		}
		seen[c.keys[i]] = struct{}{}
		merged = append(merged, item)
		appended = true
	}
	if !appended {
		return responseCompletedOutputFromPayload(terminal)
	}
	out := make([]byte, 0, len(listed.Raw)+64*len(c.items))
	out = append(out, '[')
	out = append(out, bytes.Join(merged, []byte{','})...)
	return append(out, ']')
}

func websocketOutputItemKey(item gjson.Result) string {
	if id := strings.TrimSpace(item.Get("id").String()); id != "" {
		return id
	}
	return item.Raw
}

// responsesWebsocketPrewarmPayloads answers Codex's prewarm request
// (generate=false) locally, with the shape the upstream CLIProxyAPI uses.
//
// The reply must end with response.completed. Codex 0.110 and later do not
// recognise response.done: with it the prewarm times out after about 15 seconds
// and every reply after it waits out the idle timeout before Codex retries and
// falls back to HTTP (docs/issue/evidence/20261105-codex-ws-response-done in the
// CliProxy repository).
func responsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, bool) {
	if !gjson.GetBytes(requestJSON, "generate").Exists() || gjson.GetBytes(requestJSON, "generate").Bool() {
		return nil, false
	}

	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())
	created := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`)
	payloads := [][]byte{created, completed}
	for i := range payloads {
		payloads[i], _ = sjson.SetBytes(payloads[i], "response.id", responseID)
		payloads[i], _ = sjson.SetBytes(payloads[i], "response.created_at", createdAt)
		if modelName != "" {
			payloads[i], _ = sjson.SetBytes(payloads[i], "response.model", modelName)
		}
	}
	return payloads, true
}

// writeResponsesWebsocketPreviousResponseNotFound tells the client that the
// response its increment names is not the one this connection can continue.
func writeResponsesWebsocketPreviousResponseNotFound(conn *websocket.Conn, request []byte) ([]byte, error) {
	previousID := strings.TrimSpace(gjson.GetBytes(request, "previous_response_id").String())
	data, err := json.Marshal(map[string]any{
		"type":   wsEventTypeError,
		"status": http.StatusBadRequest,
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    wsErrorCodePreviousResponseNotFound,
			"param":   "previous_response_id",
			"message": fmt.Sprintf("Previous response %q is not the last response on this connection; send the full input.", previousID),
		},
	})
	if err != nil {
		return nil, err
	}
	return data, conn.WriteMessage(websocket.TextMessage, data)
}

// closeResponsesWebsocket ends the connection between turns. Codex opens a new
// connection for its next request and sends the full input there, so nothing is
// lost; only that one request is as large as it would be over HTTP.
func closeResponsesWebsocket(conn *websocket.Conn, reason string) {
	message := websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason)
	_ = conn.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second))
}

func isWebsocketReadTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

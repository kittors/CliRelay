package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/tidwall/gjson"
)

const wsScriptModel = "ws-script-model"

// wsScriptExecutor answers each turn with the next scripted stream and keeps the
// payload every turn reached the executor with.
type wsScriptExecutor struct {
	provider string
	scripts  [][]string

	mu       sync.Mutex
	payloads [][]byte
}

func newWSScriptExecutor(t *testing.T, scripts ...[]string) *wsScriptExecutor {
	return &wsScriptExecutor{provider: "ws-script-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()), scripts: scripts}
}

func (e *wsScriptExecutor) Identifier() string { return e.provider }

func (e *wsScriptExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsScriptExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	call := len(e.payloads)
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	events := []string{fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","output":[]}}`, call+1)}
	if call < len(e.scripts) {
		events = e.scripts[call]
	}
	e.mu.Unlock()
	chunks := make(chan coreexecutor.StreamChunk, len(events))
	for _, event := range events {
		chunks <- coreexecutor.StreamChunk{Payload: []byte("data: " + event)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *wsScriptExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *wsScriptExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *wsScriptExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

func (e *wsScriptExecutor) upstream(t *testing.T) [][]byte {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]byte(nil), e.payloads...)
}

// newWSScriptServer serves ResponsesWebsocket with exec as the only account and
// returns the WebSocket URL.
func newWSScriptServer(t *testing.T, exec *wsScriptExecutor, cfg *sdkconfig.SDKConfig) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	auth := &coreauth.Auth{ID: "auth-" + exec.provider, Provider: exec.provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, exec.provider, []*registry.ModelInfo{{ID: wsScriptModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	if cfg == nil {
		cfg = &sdkconfig.SDKConfig{}
	}
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager))
	engine := gin.New()
	engine.GET("/v1/responses", h.ResponsesWebsocket)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/responses"
}

func wsScriptDial(t *testing.T, url string, dialer *websocket.Dialer) *websocket.Conn {
	t.Helper()
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.Dial(url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// wsScriptTurn sends one frame and returns the events up to the one ending the turn.
func wsScriptTurn(t *testing.T, conn *websocket.Conn, frame string) []string {
	t.Helper()
	wsTurnSend(t, conn, frame)
	var events []string
	for {
		event := string(wsTurnRead(t, conn))
		events = append(events, event)
		switch gjson.Get(event, "type").String() {
		case "response.completed", "response.incomplete", "response.failed", "error":
			return events
		}
	}
}

func wsScriptCreate(input string, extra string) string {
	frame := fmt.Sprintf(`{"type":"response.create","model":%q,"input":%s}`, wsScriptModel, input)
	if extra != "" {
		frame = strings.TrimSuffix(frame, "}") + "," + extra + "}"
	}
	return frame
}

func wsScriptUser(id string) string {
	return fmt.Sprintf(`{"type":"message","role":"user","id":%q,"content":[{"type":"input_text","text":"hi"}]}`, id)
}

func inputIDs(payload []byte) []string {
	var ids []string
	for _, item := range gjson.GetBytes(payload, "input").Array() {
		ids = append(ids, item.Get("id").String())
	}
	return ids
}

func TestResponsesWebsocketTranscriptClassify(t *testing.T) {
	held := &responsesWebsocketTranscript{
		lastRequest:        []byte(`{"model":"m","input":[{"id":"u1"}]}`),
		lastResponseOutput: []byte(`[]`),
		lastResponseID:     "resp_1",
		lastWindowID:       "w:0",
	}
	cases := []struct {
		name        string
		transcript  *responsesWebsocketTranscript
		payload     string
		passthrough bool
		want        wsTurn
	}{
		{"first request", newResponsesWebsocketTranscript(), `{"type":"response.create","previous_response_id":"resp_x"}`, false, wsTurnFollowUp},
		{"v1 append", held, `{"type":"response.append","input":[]}`, false, wsTurnFollowUp},
		{"complete request", held, `{"type":"response.create","input":[]}`, false, wsTurnNewRoot},
		{"increment on the last response", held, `{"type":"response.create","previous_response_id":"resp_1","input":[]}`, false, wsTurnFollowUp},
		{"increment on another response", held, `{"type":"response.create","previous_response_id":"resp_9","input":[]}`, false, wsTurnUnknownPrevious},
		{"upstream keeps the context", held, `{"type":"response.create","previous_response_id":"resp_9","input":[]}`, true, wsTurnFollowUp},
		{"new context window", held, `{"type":"response.create","previous_response_id":"resp_1","client_metadata":{"x-codex-window-id":"w:1"},"input":[]}`, false, wsTurnStaleRoot},
		{"window from turn metadata", held, `{"type":"response.create","previous_response_id":"resp_1","client_metadata":{"x-codex-turn-metadata":"{\"window_id\":\"w:2\"}"},"input":[]}`, true, wsTurnStaleRoot},
		{"same window", held, `{"type":"response.create","previous_response_id":"resp_1","client_metadata":{"x-codex-window-id":"w:0"},"input":[]}`, false, wsTurnFollowUp},
		{"compaction replay", held, `{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"compaction","encrypted_content":"x"}]}`, false, wsTurnStaleRoot},
	}
	for _, tc := range cases {
		if got := tc.transcript.classify([]byte(tc.payload), tc.passthrough); got != tc.want {
			t.Errorf("%s: classify = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestWebsocketOutputCollectorAddsAnnouncedItems(t *testing.T) {
	var collected websocketOutputCollector
	collected.observe([]byte(`{"type":"response.output_item.done","item":{"id":"a","type":"message"}}`))
	collected.observe([]byte(`{"type":"response.output_item.done","item":{"id":"b","type":"function_call"}}`))
	collected.observe([]byte(`{"type":"response.output_item.done","item":{"id":"a","type":"message"}}`))
	collected.observe([]byte(`{"type":"response.output_text.delta","delta":"x"}`))

	empty := collected.output([]byte(`{"type":"response.completed","response":{"output":[]}}`))
	if ids := gjson.ParseBytes(empty).Get("#.id").String(); ids != `["a","b"]` {
		t.Fatalf("empty terminal output: got %s", empty)
	}
	partial := collected.output([]byte(`{"type":"response.completed","response":{"output":[{"id":"b","type":"function_call"}]}}`))
	if ids := gjson.ParseBytes(partial).Get("#.id").String(); ids != `["b","a"]` {
		t.Fatalf("partial terminal output: got %s", partial)
	}
	full := []byte(`{"type":"response.completed","response":{"output":[ {"id":"a","type":"message"}, {"id":"b","type":"function_call"} ]}}`)
	if got := collected.output(full); !bytes.Equal(got, []byte(gjson.GetBytes(full, "response.output").Raw)) {
		t.Fatalf("a complete terminal output must be kept as sent, got %s", got)
	}
}

func TestWebsocketResponseFinished(t *testing.T) {
	for event, want := range map[string]bool{
		"response.completed":  true,
		"response.incomplete": true,
		"response.done":       true,
		"response.failed":     false,
		"response.created":    false,
	} {
		if got := websocketResponseFinished(event); got != want {
			t.Errorf("websocketResponseFinished(%q) = %v, want %v", event, got, want)
		}
	}
}

func TestAppendWebsocketEventStopsAtItsLimit(t *testing.T) {
	var log strings.Builder
	appendWebsocketEvent(&log, "request", []byte(`{"type":"response.create"}`))
	appendWebsocketEvent(&log, "request", bytes.Repeat([]byte("a"), wsSessionLogMaxBytes))
	appendWebsocketEvent(&log, "response", []byte(`{"type":"response.completed"}`))
	got := log.String()
	if strings.Count(got, wsSessionLogTruncatedMarker) != 1 || !strings.HasSuffix(got, wsSessionLogTruncatedMarker) {
		t.Fatalf("the log must end with one truncation marker, got %d bytes ending %q", len(got), got[max(0, len(got)-120):])
	}
	if len(got) > 1024 {
		t.Fatalf("the oversized event must not be kept: log is %d bytes", len(got))
	}
}

func TestResponsesWebsocketPrewarmMatchesTheResponseShape(t *testing.T) {
	payloads, ok := responsesWebsocketPrewarmPayloads([]byte(`{"model":"gpt-5.5","generate":false,"input":[]}`))
	if !ok || len(payloads) != 2 {
		t.Fatalf("expected two prewarm events, got %d", len(payloads))
	}
	for i, want := range []string{"in_progress", "completed"} {
		event := gjson.ParseBytes(payloads[i])
		if event.Get("sequence_number").Int() != int64(i) || event.Get("response.status").String() != want ||
			event.Get("response.object").String() != "response" || event.Get("response.model").String() != "gpt-5.5" ||
			event.Get("response.created_at").Int() == 0 {
			t.Fatalf("prewarm event %d has the wrong shape: %s", i, payloads[i])
		}
	}
}

func TestResponsesWebsocketExpandsAnIncrementNamingTheLastResponse(t *testing.T) {
	exec := newWSScriptExecutor(t,
		[]string{`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","id":"a1"}]}}`},
	)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	first := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	if last := first[len(first)-1]; gjson.Get(last, "type").String() != "response.completed" {
		t.Fatalf("the reply must end with response.completed as sent upstream, got %s", last)
	}
	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u2")+"]", `"previous_response_id":"resp_1"`))

	upstream := exec.upstream(t)
	if got := strings.Join(inputIDs(upstream[1]), ","); got != "u1,a1,u2" {
		t.Fatalf("the increment must be expanded with the transcript, upstream input = %s", got)
	}
	if gjson.GetBytes(upstream[1], "previous_response_id").Exists() {
		t.Fatalf("an expanded request must not name a previous response")
	}
}

func TestResponsesWebsocketTakesACompleteRequestAsItIs(t *testing.T) {
	exec := newWSScriptExecutor(t,
		[]string{`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","role":"assistant","id":"a1"}]}}`},
	)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	// Codex re-sends the whole conversation, without previous_response_id, when it
	// cannot reuse the connection's last response (a changed model, for one).
	full := "[" + wsScriptUser("u1") + `,{"type":"message","role":"assistant","id":"a1"},` + wsScriptUser("u2") + "]"
	wsScriptTurn(t, conn, wsScriptCreate(full, ""))

	if got := strings.Join(inputIDs(exec.upstream(t)[1]), ","); got != "u1,a1,u2" {
		t.Fatalf("a complete request must not be expanded again, upstream input = %s", got)
	}
}

func TestResponsesWebsocketAsksForTheFullRequestOnAnUnknownPrevious(t *testing.T) {
	exec := newWSScriptExecutor(t)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	events := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u2")+"]", `"previous_response_id":"resp_elsewhere"`))
	reply := gjson.Parse(events[len(events)-1])
	if reply.Get("type").String() != "error" || reply.Get("error.code").String() != wsErrorCodePreviousResponseNotFound {
		t.Fatalf("an increment on an unknown response must be refused with %s, got %s", wsErrorCodePreviousResponseNotFound, reply.Raw)
	}
	if n := len(exec.upstream(t)); n != 1 {
		t.Fatalf("the refused increment must not reach the executor, calls = %d", n)
	}
	// The connection stays usable: Codex sends the request again in full.
	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+","+wsScriptUser("u2")+"]", ""))
	if got := strings.Join(inputIDs(exec.upstream(t)[1]), ","); got != "u1,u2" {
		t.Fatalf("the retried full request must go upstream as sent, upstream input = %s", got)
	}
}

func TestResponsesWebsocketStartsOverOnANewContextWindow(t *testing.T) {
	exec := newWSScriptExecutor(t)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", `"client_metadata":{"x-codex-window-id":"w:0"}`))
	// After compacting, Codex opens window w:1 and sends the new window in full,
	// yet may still name the previous window's last response.
	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("s1")+","+wsScriptUser("u2")+"]",
		`"previous_response_id":"resp_1","client_metadata":{"x-codex-window-id":"w:1"}`))

	second := exec.upstream(t)[1]
	if got := strings.Join(inputIDs(second), ","); got != "s1,u2" {
		t.Fatalf("a new context window must not be appended to the old one, upstream input = %s", got)
	}
	if gjson.GetBytes(second, "previous_response_id").Exists() {
		t.Fatalf("the previous window's response id must be dropped")
	}
}

func TestResponsesWebsocketContinuesFromThePrewarm(t *testing.T) {
	exec := newWSScriptExecutor(t)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	prewarm := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", `"generate":false`))
	prewarmID := gjson.Get(prewarm[len(prewarm)-1], "response.id").String()
	if gjson.Get(prewarm[len(prewarm)-1], "type").String() != "response.completed" || prewarmID == "" {
		t.Fatalf("the prewarm must end with response.completed and an id, got %v", prewarm)
	}
	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u2")+"]", fmt.Sprintf(`"previous_response_id":%q`, prewarmID)))

	upstream := exec.upstream(t)
	if len(upstream) != 1 {
		t.Fatalf("only the business request reaches the executor, calls = %d", len(upstream))
	}
	if got := strings.Join(inputIDs(upstream[0]), ","); got != "u1,u2" {
		t.Fatalf("the request after the prewarm must carry the prewarmed input, upstream input = %s", got)
	}
}

func TestResponsesWebsocketKeepsAnnouncedItemsInTheTranscript(t *testing.T) {
	exec := newWSScriptExecutor(t, []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"exec","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","output":[]}}`,
	})
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	wsScriptTurn(t, conn, wsScriptCreate(`[{"type":"function_call_output","id":"fo1","call_id":"c1","output":"ok"}]`, `"previous_response_id":"resp_1"`))

	if got := strings.Join(inputIDs(exec.upstream(t)[1]), ","); got != "u1,fc1,fo1" {
		t.Fatalf("an item announced before an empty response.output must stay in the transcript, upstream input = %s", got)
	}
}

func TestResponsesWebsocketEndsATurnOnIncomplete(t *testing.T) {
	exec := newWSScriptExecutor(t,
		[]string{`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`},
	)
	conn := wsScriptDial(t, newWSScriptServer(t, exec, nil), nil)

	first := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	if gjson.Get(first[len(first)-1], "type").String() != "response.incomplete" {
		t.Fatalf("a cut-short reply must reach the client as response.incomplete, got %v", first)
	}
	// No error may trail it: the next frame read belongs to the next turn.
	second := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u2")+"]", `"previous_response_id":"resp_1"`))
	if gjson.Get(second[0], "type").String() == "error" {
		t.Fatalf("response.incomplete must end the turn without an error event, got %s", second[0])
	}
}

func TestResponsesWebsocketDisabledAnswers426(t *testing.T) {
	exec := newWSScriptExecutor(t)
	url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{Disabled: true}})
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("a disabled entry must answer 426, got status %d err %v", status, err)
	}
	_ = resp.Body.Close()
}

func TestResponsesWebsocketClosesPastTheTranscriptLimit(t *testing.T) {
	exec := newWSScriptExecutor(t)
	url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{MaxTranscriptMB: 1}})
	conn := wsScriptDial(t, url, nil)

	big := fmt.Sprintf(`[{"type":"message","role":"user","id":"u1","content":[{"type":"input_text","text":%q}]}]`, strings.Repeat("x", 1<<20+1024))
	events := wsScriptTurn(t, conn, wsScriptCreate(big, ""))
	if gjson.Get(events[len(events)-1], "type").String() != "response.completed" {
		t.Fatalf("the turn must be answered before the connection closes, got %v", events)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("the connection must close normally once the transcript is over the limit, got %v", err)
	}
}

func TestResponsesWebsocketClosesAnIdleConnection(t *testing.T) {
	exec := newWSScriptExecutor(t)
	url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{IdleTimeoutSeconds: 1}})
	conn := wsScriptDial(t, url, nil)

	wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err := conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseNormalClosure || closeErr.Text != "idle timeout" {
		t.Fatalf("an idle connection must be closed normally, got %v", err)
	}
}

func TestResponsesWebsocketRefusesAnOversizedMessage(t *testing.T) {
	exec := newWSScriptExecutor(t)
	url := newWSScriptServer(t, exec, &sdkconfig.SDKConfig{ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{MaxMessageMB: 1}})
	conn := wsScriptDial(t, url, nil)

	big := fmt.Sprintf(`[{"type":"message","role":"user","id":"u1","content":[{"type":"input_text","text":%q}]}]`, strings.Repeat("x", 1<<20+1024))
	_ = conn.WriteMessage(websocket.TextMessage, []byte(wsScriptCreate(big, "")))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatalf("an oversized message must close the connection")
	}
	if n := len(exec.upstream(t)); n != 0 {
		t.Fatalf("an oversized message must not reach the executor, calls = %d", n)
	}
}

func TestResponsesWebsocketNegotiatesCompression(t *testing.T) {
	off := false
	for name, cfg := range map[string]*sdkconfig.SDKConfig{
		"default":    nil,
		"turned off": {ResponsesWebsocket: sdkconfig.ResponsesWebsocketConfig{Compression: &off}},
	} {
		exec := newWSScriptExecutor(t)
		url := newWSScriptServer(t, exec, cfg)
		conn, resp, err := (&websocket.Dialer{EnableCompression: true}).Dial(url, nil)
		if err != nil {
			t.Fatalf("%s: dial: %v", name, err)
		}
		negotiated := strings.Contains(resp.Header.Get("Sec-Websocket-Extensions"), "permessage-deflate")
		_ = resp.Body.Close()
		if negotiated != (name == "default") {
			t.Fatalf("%s: permessage-deflate negotiated = %v", name, negotiated)
		}
		events := wsScriptTurn(t, conn, wsScriptCreate("["+wsScriptUser("u1")+"]", ""))
		if gjson.Get(events[len(events)-1], "type").String() != "response.completed" {
			t.Fatalf("%s: a turn must complete over the connection, got %v", name, events)
		}
		_ = conn.Close()
	}
}

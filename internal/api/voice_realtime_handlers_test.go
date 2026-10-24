package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkConfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func newVoiceTestServer(t *testing.T, authEntries ...*cliproxyauth.Auth) *Server {
	t.Helper()
	gin.SetMode(gin.TestMode)

	authManager := cliproxyauth.NewManager(nil, nil, nil)
	ctx := context.Background()
	for _, entry := range authEntries {
		if _, err := authManager.Register(ctx, entry); err != nil {
			t.Fatalf("register auth %s: %v", entry.ID, err)
		}
	}

	cfg := &config.Config{
		SDKConfig: sdkConfig.SDKConfig{
			APIKeys: []string{"test-client-key"},
		},
		Port:                   0,
		AuthDir:                t.TempDir(),
		UsageStatisticsEnabled: false,
	}
	accessManager := access.NewManager()
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	return NewServer(cfg, authManager, accessManager, configPath)
}

func TestGrokVoiceSessionAndWebsocketRelay(t *testing.T) {
	var upstreamCookie atomic.Value
	var upstreamQuery atomic.Value

	upstreamWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCookie.Store(r.Header.Get("Cookie"))
		upstreamQuery.Store(r.URL.RawQuery)
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			mt, msg, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			_ = conn.WriteMessage(mt, append([]byte("grok-echo:"), msg...))
		}
	}))
	defer upstreamWS.Close()

	wsEndpoint := "ws" + strings.TrimPrefix(upstreamWS.URL, "http") + "/ws/app_chat/stream_audio"
	t.Setenv("CLIRELAY_GROK_VOICE_WS_URL", wsEndpoint)

	srv := newVoiceTestServer(t, &cliproxyauth.Auth{
		ID:       "xai-sub-1",
		Provider: "xai",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{
			"sso_token": "xai-sso-secret-token",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/voice/grok/session", strings.NewReader(`{"voice":"eve"}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v1/voice/grok/session, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionResp); err != nil {
		t.Fatalf("unmarshal session response: %v", err)
	}
	if sessionResp["voice"] != "eve" {
		t.Fatalf("expected voice=eve, got %v", sessionResp["voice"])
	}
	if sessionResp["credential_id"] != "xai-sub-1" {
		t.Fatalf("expected credential_id=xai-sub-1, got %v", sessionResp["credential_id"])
	}

	gatewayHTTP := httptest.NewServer(srv.engine)
	defer gatewayHTTP.Close()

	wsURL := "ws" + strings.TrimPrefix(gatewayHTTP.URL, "http") + "/v1/voice/grok?voice=rex"
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	clientConn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if wsResp != nil && wsResp.Body != nil {
		defer func() { _ = wsResp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("dial /v1/voice/grok: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte("hello-grok")); err != nil {
		t.Fatalf("write message: %v", err)
	}
	_, reply, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(reply) != "grok-echo:hello-grok" {
		t.Fatalf("unexpected reply: %s", string(reply))
	}

	gotCookie, _ := upstreamCookie.Load().(string)
	if !strings.Contains(gotCookie, "sso=xai-sso-secret-token") {
		t.Fatalf("expected upstream Cookie with sso token, got %q", gotCookie)
	}
	gotQuery, _ := upstreamQuery.Load().(string)
	for _, expectedParam := range []string{"model=xai", "client=oai", "voice=rex", "enable_markdown_transcript=true"} {
		if !strings.Contains(gotQuery, expectedParam) {
			t.Fatalf("expected upstream query %q to contain %q", gotQuery, expectedParam)
		}
	}
}

func TestClaudeVoiceSessionAndSTTWebsocketRelay(t *testing.T) {
	var upstreamAuth atomic.Value
	var upstreamXApp atomic.Value
	var upstreamUA atomic.Value
	var upstreamQuery atomic.Value

	upstreamWS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth.Store(r.Header.Get("Authorization"))
		upstreamXApp.Store(r.Header.Get("x-app"))
		upstreamUA.Store(r.Header.Get("User-Agent"))
		upstreamQuery.Store(r.URL.RawQuery)
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			mt, msg, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			_ = conn.WriteMessage(mt, append([]byte("claude-stt:"), msg...))
		}
	}))
	defer upstreamWS.Close()

	wsEndpoint := "ws" + strings.TrimPrefix(upstreamWS.URL, "http") + "/api/ws/speech_to_text/voice_stream"
	t.Setenv("CLIRELAY_CLAUDE_VOICE_WS_URL", wsEndpoint)

	srv := newVoiceTestServer(t, &cliproxyauth.Auth{
		ID:       "claude-sub-1",
		Provider: "claude",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "sk-ant-oat01-pooled-token",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/voice/claude/session", strings.NewReader(`{"model":"claude-opus-4-7","voice":"Mellow","keyterms":["CliRelay","useEffect"]}`))
	req.Header.Set("Authorization", "Bearer test-client-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for /v1/voice/claude/session, got %d: %s", rec.Code, rec.Body.String())
	}
	var sessionResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &sessionResp); err != nil {
		t.Fatalf("unmarshal claude session response: %v", err)
	}
	if sessionResp["model"] != "claude-opus-4-7" || sessionResp["voice"] != "Mellow" {
		t.Fatalf("unexpected claude session payload: %v", sessionResp)
	}
	if sessionResp["websocket_path"] != "/api/ws/speech_to_text/voice_stream" {
		t.Fatalf("unexpected websocket_path: %v", sessionResp["websocket_path"])
	}

	fableReq := httptest.NewRequest(http.MethodPost, "/v1/voice/claude/session", strings.NewReader(`{"model":"claude-fable"}`))
	fableReq.Header.Set("Authorization", "Bearer test-client-key")
	fableReq.Header.Set("Content-Type", "application/json")
	fableRec := httptest.NewRecorder()
	srv.engine.ServeHTTP(fableRec, fableReq)
	if fableRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for excluded Fable model, got %d: %s", fableRec.Code, fableRec.Body.String())
	}

	gatewayHTTP := httptest.NewServer(srv.engine)
	defer gatewayHTTP.Close()

	wsURL := "ws" + strings.TrimPrefix(gatewayHTTP.URL, "http") +
		"/api/ws/speech_to_text/voice_stream?encoding=linear16&sample_rate=16000&keyterms=CliRelay&keyterms=useEffect"
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	clientConn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if wsResp != nil && wsResp.Body != nil {
		defer func() { _ = wsResp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("dial /api/ws/speech_to_text/voice_stream: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	if err := clientConn.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02, 0x03, 0x04}); err != nil {
		t.Fatalf("write binary audio: %v", err)
	}
	_, reply, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("read stt reply: %v", err)
	}
	if !strings.HasPrefix(string(reply), "claude-stt:") {
		t.Fatalf("unexpected stt reply: %q", string(reply))
	}

	if gotAuth, _ := upstreamAuth.Load().(string); gotAuth != "Bearer sk-ant-oat01-pooled-token" {
		t.Fatalf("expected pooled Claude OAuth Bearer header, got %q", gotAuth)
	}
	if gotXApp, _ := upstreamXApp.Load().(string); gotXApp != "cli" {
		t.Fatalf("expected x-app=cli header, got %q", gotXApp)
	}
	if gotUA, _ := upstreamUA.Load().(string); !strings.HasPrefix(gotUA, "claude-cli/") {
		t.Fatalf("expected claude-cli User-Agent, got %q", gotUA)
	}
	gotQuery, _ := upstreamQuery.Load().(string)
	for _, expectedParam := range []string{"encoding=linear16", "sample_rate=16000", "keyterms=CliRelay", "keyterms=useEffect"} {
		if !strings.Contains(gotQuery, expectedParam) {
			t.Fatalf("expected upstream query %q to contain %q", gotQuery, expectedParam)
		}
	}
}

func TestChatGPTLiveOfferAndSidebandWebsocketRelay(t *testing.T) {
	var offerAuthHeader atomic.Value
	var offerAccountHeader atomic.Value
	var sidebandAuthHeader atomic.Value
	var sidebandAccountHeader atomic.Value
	var sidebandPath atomic.Value

	upstreamLive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/v1/live" {
			offerAuthHeader.Store(r.Header.Get("Authorization"))
			offerAccountHeader.Store(r.Header.Get("ChatGPT-Account-Id"))
			w.Header().Set("Location", "https://chatgpt.com/backend-api/codex/v1/live/rtc_call_999")
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=answer\r\n"))
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			sidebandAuthHeader.Store(r.Header.Get("Authorization"))
			sidebandAccountHeader.Store(r.Header.Get("ChatGPT-Account-Id"))
			sidebandPath.Store(r.URL.Path)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			for {
				mt, msg, readErr := conn.ReadMessage()
				if readErr != nil {
					return
				}
				_ = conn.WriteMessage(mt, append([]byte("sideband:"), msg...))
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstreamLive.Close()

	t.Setenv("CLIRELAY_CHATGPT_LIVE_URL", upstreamLive.URL+"/backend-api/codex/v1/live")
	t.Setenv("CLIRELAY_CHATGPT_LIVE_WS_URL", "ws"+strings.TrimPrefix(upstreamLive.URL, "http")+"/backend-api/codex/v1/live")

	srv := newVoiceTestServer(t, &cliproxyauth.Auth{
		ID:       "codex-sub-1",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{
			"access_token": "chatgpt-plus-oauth-token",
			"account_id":   "acct_plus_42",
		},
	})

	offerReq := httptest.NewRequest(http.MethodPost, "/v1/live", strings.NewReader(`{"sdp":"v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n","model":"gpt-realtime"}`))
	offerReq.Header.Set("Authorization", "Bearer test-client-key")
	offerReq.Header.Set("Content-Type", "application/json")
	offerReq.Header.Set("Accept", "application/json")
	offerRec := httptest.NewRecorder()
	srv.engine.ServeHTTP(offerRec, offerReq)

	if offerRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 from POST /v1/live, got %d: %s", offerRec.Code, offerRec.Body.String())
	}
	if offerRec.Header().Get("X-Call-Id") != "rtc_call_999" {
		t.Fatalf("expected X-Call-Id=rtc_call_999, got %q", offerRec.Header().Get("X-Call-Id"))
	}
	if gotAuth, _ := offerAuthHeader.Load().(string); gotAuth != "Bearer chatgpt-plus-oauth-token" {
		t.Fatalf("expected Bearer chatgpt-plus-oauth-token on offer, got %q", gotAuth)
	}
	if gotAcct, _ := offerAccountHeader.Load().(string); gotAcct != "acct_plus_42" {
		t.Fatalf("expected ChatGPT-Account-Id=acct_plus_42 on offer, got %q", gotAcct)
	}

	gatewayHTTP := httptest.NewServer(srv.engine)
	defer gatewayHTTP.Close()

	wsURL := "ws" + strings.TrimPrefix(gatewayHTTP.URL, "http") + "/v1/live/rtc_call_999"
	headers := http.Header{"Authorization": []string{"Bearer test-client-key"}}
	sidebandConn, wsResp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if wsResp != nil && wsResp.Body != nil {
		defer func() { _ = wsResp.Body.Close() }()
	}
	if err != nil {
		t.Fatalf("dial /v1/live/rtc_call_999: %v", err)
	}
	defer func() { _ = sidebandConn.Close() }()

	if err := sidebandConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write sideband message: %v", err)
	}
	_, sbReply, err := sidebandConn.ReadMessage()
	if err != nil {
		t.Fatalf("read sideband message: %v", err)
	}
	if string(sbReply) != `sideband:{"type":"response.create"}` {
		t.Fatalf("unexpected sideband reply: %s", string(sbReply))
	}
	if gotPath, _ := sidebandPath.Load().(string); gotPath != "/backend-api/codex/v1/live/rtc_call_999" {
		t.Fatalf("unexpected upstream sideband path: %q", gotPath)
	}
	if gotAcct, _ := sidebandAccountHeader.Load().(string); gotAcct != "acct_plus_42" {
		t.Fatalf("expected bound ChatGPT-Account-Id=acct_plus_42 on sideband, got %q", gotAcct)
	}
}

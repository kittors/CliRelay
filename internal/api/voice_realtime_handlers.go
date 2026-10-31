package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type activeVoiceCall struct {
	CallID    string
	AuthID    string
	Token     string
	AccountID string
	CreatedAt time.Time
}

var (
	voiceCallsMu    sync.RWMutex
	activeLiveCalls = make(map[string]activeVoiceCall)
	voiceUpgrader   = websocket.Upgrader{
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
		CheckOrigin:     func(_ *http.Request) bool { return true },
	}
)

func recordActiveVoiceCall(call activeVoiceCall) {
	if strings.TrimSpace(call.CallID) == "" {
		return
	}
	now := time.Now()
	voiceCallsMu.Lock()
	defer voiceCallsMu.Unlock()
	for id, existing := range activeLiveCalls {
		if now.Sub(existing.CreatedAt) > 2*time.Hour {
			delete(activeLiveCalls, id)
		}
	}
	call.CreatedAt = now
	activeLiveCalls[call.CallID] = call
}

func lookupActiveVoiceCall(callID string) (activeVoiceCall, bool) {
	voiceCallsMu.RLock()
	defer voiceCallsMu.RUnlock()
	c, ok := activeLiveCalls[strings.TrimSpace(callID)]
	if !ok || time.Since(c.CreatedAt) > 2*time.Hour {
		return activeVoiceCall{}, false
	}
	return c, true
}

func extractAuthString(a *cliproxyauth.Auth, keys ...string) string {
	if a == nil {
		return ""
	}
	for _, k := range keys {
		if a.Attributes != nil {
			if v := strings.TrimSpace(a.Attributes[k]); v != "" {
				return v
			}
		}
		if a.Metadata != nil {
			if raw, ok := a.Metadata[k]; ok {
				switch val := raw.(type) {
				case string:
					if trimmed := strings.TrimSpace(val); trimmed != "" {
						return trimmed
					}
				case map[string]any:
					for _, subKey := range keys {
						if sub, ok := val[subKey].(string); ok && strings.TrimSpace(sub) != "" {
							return strings.TrimSpace(sub)
						}
					}
				}
			}
			if tokenMap, ok := a.Metadata["token"].(map[string]any); ok {
				if v, ok := tokenMap[k].(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

func (s *Server) selectProviderVoiceAuth(providers ...string) *cliproxyauth.Auth {
	if s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		return nil
	}
	auths := s.handlers.AuthManager.List()
	wanted := make(map[string]struct{}, len(providers))
	for _, p := range providers {
		if normalized := strings.ToLower(strings.TrimSpace(p)); normalized != "" {
			wanted[normalized] = struct{}{}
		}
	}
	now := time.Now()
	var fallback *cliproxyauth.Auth
	for _, a := range auths {
		if a == nil || a.Disabled {
			continue
		}
		prov := strings.ToLower(strings.TrimSpace(a.Provider))
		if _, ok := wanted[prov]; !ok {
			continue
		}
		if fallback == nil {
			fallback = a
		}
		if a.Unavailable && a.NextRetryAfter.After(now) {
			continue
		}
		if a.Status == cliproxyauth.StatusError || a.Status == cliproxyauth.StatusDisabled {
			continue
		}
		return a
	}
	return fallback
}

func extractVoiceBearerToken(a *cliproxyauth.Auth) string {
	return extractAuthString(a, "access_token", "api_key", "token", "sso_token", "sso", "session_token")
}

func extractVoiceAccountID(a *cliproxyauth.Auth) string {
	return extractAuthString(a, "account_id", "chatgpt_account_id", "organization_id")
}

func extractGrokCookie(a *cliproxyauth.Auth) string {
	return extractAuthString(a, "cookie", "sso_cookie", "sso_token", "sso")
}

func extractCallIDFromLocation(loc string) string {
	trimmed := strings.TrimSpace(loc)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 && idx+1 < len(trimmed) {
		return trimmed[idx+1:]
	}
	return trimmed
}

func (s *Server) handleGrokVoiceSession(c *gin.Context) {
	authEntry := s.selectProviderVoiceAuth("xai", "grok")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active xAI/Grok voice credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}

	var body struct {
		Voice                    string `json:"voice"`
		Language                 string `json:"language"`
		Timezone                 string `json:"timezone"`
		EnableSearch             *bool  `json:"enable_search"`
		EnableMarkdownTranscript *bool  `json:"enable_markdown_transcript"`
	}
	_ = c.ShouldBindJSON(&body)

	overrides := map[string]string{}
	if body.Voice != "" {
		overrides["voice"] = normalizeGrokVoice(body.Voice)
	}
	if body.Language != "" {
		overrides["x-app-language"] = strings.TrimSpace(body.Language)
	}
	if body.Timezone != "" {
		overrides["x-app-timezone"] = strings.TrimSpace(body.Timezone)
	}
	if body.EnableSearch != nil {
		overrides["enable_search"] = fmt.Sprintf("%v", *body.EnableSearch)
	}
	if body.EnableMarkdownTranscript != nil {
		overrides["enable_markdown_transcript"] = fmt.Sprintf("%v", *body.EnableMarkdownTranscript)
	}

	upstreamURL, err := buildGrokVoiceUpstreamURL(c.Request.URL.Query(), overrides)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	voice := normalizeGrokVoice(overrides["voice"])
	if voice == "" {
		voice = normalizeGrokVoice(c.Query("voice"))
	}

	c.JSON(http.StatusOK, gin.H{
		"object":            "voice.session",
		"provider":          "xai",
		"model":             "xai",
		"voice":             voice,
		"voices":            grokSupportedVoices,
		"websocket_path":    "/v1/voice/grok?voice=" + voice,
		"upstream_endpoint": upstreamURL,
		"credential_id":     authEntry.ID,
	})
}

func (s *Server) handleGrokVoiceWebsocket(c *gin.Context) {
	clearServerWriteDeadline(c)
	authEntry := s.selectProviderVoiceAuth("xai", "grok")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active xAI/Grok voice credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}

	upstreamURL, err := buildGrokVoiceUpstreamURL(c.Request.URL.Query(), nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	token := extractVoiceBearerToken(authEntry)
	cookie := extractGrokCookie(authEntry)
	headers := buildGrokVoiceHeaders(token, cookie)

	s.bridgeWebSocket(c, upstreamURL, headers)
}

func (s *Server) handleClaudeVoiceSession(c *gin.Context) {
	var body struct {
		Model                 string   `json:"model"`
		Voice                 string   `json:"voice"`
		Language              string   `json:"language"`
		Encoding              string   `json:"encoding"`
		SampleRate            int      `json:"sample_rate"`
		Channels              int      `json:"channels"`
		EndpointingMs         int      `json:"endpointing_ms"`
		UtteranceEndMs        int      `json:"utterance_end_ms"`
		UseConversationEngine *bool    `json:"use_conversation_engine"`
		STTProvider           string   `json:"stt_provider"`
		Keyterms              []string `json:"keyterms"`
	}
	_ = c.ShouldBindJSON(&body)

	requestedModel := strings.TrimSpace(body.Model)
	if requestedModel == "" {
		requestedModel = strings.TrimSpace(c.Query("model"))
	}
	if requestedModel != "" && isClaudeModelExcludedFromVoice(requestedModel) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Model '%s' is not available in Claude voice mode. Supported models: Sonnet 4.6, Opus 4.7, Haiku 4.5.", requestedModel),
				"type":    "invalid_request_error",
				"code":    "voice_model_not_supported",
			},
		})
		return
	}
	if requestedModel == "" {
		requestedModel = "claude-sonnet-4-6"
	}

	authEntry := s.selectProviderVoiceAuth("claude", "anthropic")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active Claude OAuth voice credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}

	overrides := map[string]string{}
	if body.Language != "" {
		overrides["language"] = strings.TrimSpace(body.Language)
	}
	if body.Encoding != "" {
		overrides["encoding"] = strings.TrimSpace(body.Encoding)
	}
	if body.SampleRate > 0 {
		overrides["sample_rate"] = fmt.Sprintf("%d", body.SampleRate)
	}
	if body.Channels > 0 {
		overrides["channels"] = fmt.Sprintf("%d", body.Channels)
	}
	if body.EndpointingMs > 0 {
		overrides["endpointing_ms"] = fmt.Sprintf("%d", body.EndpointingMs)
	}
	if body.UtteranceEndMs > 0 {
		overrides["utterance_end_ms"] = fmt.Sprintf("%d", body.UtteranceEndMs)
	}
	if body.UseConversationEngine != nil {
		overrides["use_conversation_engine"] = fmt.Sprintf("%v", *body.UseConversationEngine)
	}
	if body.STTProvider != "" {
		overrides["stt_provider"] = strings.TrimSpace(body.STTProvider)
	}

	upstreamURL, err := buildClaudeVoiceUpstreamURL(c.Request.URL.Query(), overrides, body.Keyterms)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	voice := strings.TrimSpace(body.Voice)
	if voice == "" {
		voice = strings.TrimSpace(c.Query("voice"))
	}
	if voice == "" {
		voice = "Airy"
	}

	c.JSON(http.StatusOK, gin.H{
		"object":               "voice.session",
		"provider":             "anthropic",
		"protocol":             "claude-voice-stream",
		"model":                requestedModel,
		"supported_models":     claudeSupportedVoiceModels,
		"excluded_models":      []string{"claude-fable"},
		"voice":                voice,
		"voices":               claudeSupportedVoices,
		"websocket_path":       "/api/ws/speech_to_text/voice_stream",
		"alias_websocket_path": "/v1/voice/claude",
		"upstream_endpoint":    upstreamURL,
		"credential_id":        authEntry.ID,
	})
}

func (s *Server) handleClaudeVoiceWebsocket(c *gin.Context) {
	clearServerWriteDeadline(c)
	authEntry := s.selectProviderVoiceAuth("claude", "anthropic")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active Claude OAuth voice credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}

	token := extractVoiceBearerToken(authEntry)
	if token == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "Selected Claude credential does not contain an OAuth access token",
				"type":    "service_unavailable",
			},
		})
		return
	}

	upstreamURL, err := buildClaudeVoiceUpstreamURL(c.Request.URL.Query(), nil, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	headers := buildClaudeVoiceHeaders(token, c.GetHeader("User-Agent"))
	s.bridgeWebSocket(c, upstreamURL, headers)
}

func (s *Server) handleChatGPTLiveOffer(c *gin.Context) {
	authEntry := s.selectProviderVoiceAuth("codex", "openai")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active ChatGPT/Codex voice credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}

	token := extractVoiceBearerToken(authEntry)
	if token == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "Selected ChatGPT/Codex credential does not contain an access token",
				"type":    "service_unavailable",
			},
		})
		return
	}
	accountID := extractVoiceAccountID(authEntry)

	sdpOffer, sessionJSON, err := parseChatGPTLiveOffer(c.Request)
	if err != nil || strings.TrimSpace(sdpOffer) == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "WebRTC SDP offer is required (send multipart/form-data, application/sdp, or JSON {\"sdp\": \"...\"})",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	multipartBody, contentType, err := buildChatGPTLiveMultipartBody(sdpOffer, sessionJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, resolveChatGPTLiveEndpoint(), bytes.NewReader(multipartBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/sdp, application/json")
	req.Header.Set("Origin", "https://chatgpt.com")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": err.Error(), "type": "bad_gateway"}})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	location := strings.TrimSpace(resp.Header.Get("Location"))
	callID := strings.TrimSpace(resp.Header.Get("X-Call-Id"))
	if callID == "" {
		callID = extractCallIDFromLocation(location)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && callID == "" && len(respBytes) > 0 && respBytes[0] == '{' {
		var parsed map[string]any
		if json.Unmarshal(respBytes, &parsed) == nil {
			if id, ok := parsed["call_id"].(string); ok && strings.TrimSpace(id) != "" {
				callID = strings.TrimSpace(id)
			} else if id, ok := parsed["id"].(string); ok && strings.TrimSpace(id) != "" {
				callID = strings.TrimSpace(id)
			}
		}
	}

	if callID != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		recordActiveVoiceCall(activeVoiceCall{
			CallID:    callID,
			AuthID:    authEntry.ID,
			Token:     token,
			AccountID: accountID,
		})
		c.Header("Location", "/v1/live/"+callID)
		c.Header("X-Call-Id", callID)
	} else if location != "" {
		c.Header("Location", location)
	}

	wantsJSON := strings.Contains(strings.ToLower(c.GetHeader("Accept")), "application/json") ||
		strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "application/json")
	if wantsJSON && resp.StatusCode >= 200 && resp.StatusCode < 300 && !(len(respBytes) > 0 && respBytes[0] == '{') {
		c.JSON(resp.StatusCode, gin.H{
			"object":             "realtime.call",
			"call_id":            callID,
			"sdp":                string(respBytes),
			"location":           "/v1/live/" + callID,
			"sideband_websocket": "/v1/live/" + callID,
		})
		return
	}

	respCT := resp.Header.Get("Content-Type")
	if respCT == "" {
		respCT = "application/sdp"
	}
	c.Data(resp.StatusCode, respCT, respBytes)
}

func (s *Server) handleChatGPTLiveSidebandWebsocket(c *gin.Context) {
	clearServerWriteDeadline(c)
	callID := strings.TrimSpace(c.Param("call_id"))
	if callID == "" {
		callID = strings.TrimSpace(c.Query("call_id"))
	}
	if callID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": "call_id is required for GPT-Live sideband WebSocket (/v1/live/:call_id)",
				"type":    "invalid_request_error",
			},
		})
		return
	}

	var token, accountID string
	if bound, ok := lookupActiveVoiceCall(callID); ok && bound.Token != "" {
		token = bound.Token
		accountID = bound.AccountID
	} else {
		authEntry := s.selectProviderVoiceAuth("codex", "openai")
		if authEntry == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{
					"message": "No active ChatGPT/Codex voice credentials available in pool",
					"type":    "service_unavailable",
				},
			})
			return
		}
		token = extractVoiceBearerToken(authEntry)
		accountID = extractVoiceAccountID(authEntry)
	}

	upstreamURL, err := buildChatGPTLiveSidebandURL(callID, c.Request.URL.Query())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Origin", "https://chatgpt.com")
	if accountID != "" {
		headers.Set("ChatGPT-Account-Id", accountID)
	}

	s.bridgeWebSocket(c, upstreamURL, headers)
}

func (s *Server) handleRealtimeWebsocket(c *gin.Context) {
	model := strings.ToLower(strings.TrimSpace(c.Query("model")))
	provider := strings.ToLower(strings.TrimSpace(c.Query("provider")))
	if provider == "xai" || provider == "grok" || model == "xai" || strings.HasPrefix(model, "grok") {
		s.handleGrokVoiceWebsocket(c)
		return
	}
	if provider == "claude" || provider == "anthropic" || strings.HasPrefix(model, "claude") {
		s.handleClaudeVoiceWebsocket(c)
		return
	}
	if callID := strings.TrimSpace(c.Query("call_id")); callID != "" {
		s.handleChatGPTLiveSidebandWebsocket(c)
		return
	}

	clearServerWriteDeadline(c)
	authEntry := s.selectProviderVoiceAuth("codex", "openai")
	if authEntry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"message": "No active OpenAI/Codex realtime credentials available in pool",
				"type":    "service_unavailable",
			},
		})
		return
	}
	token := extractVoiceBearerToken(authEntry)
	accountID := extractVoiceAccountID(authEntry)

	base := resolveOpenAIRealtimeEndpoint()
	if rawQ := c.Request.URL.RawQuery; rawQ != "" {
		if strings.Contains(base, "?") {
			base += "&" + rawQ
		} else {
			base += "?" + rawQ
		}
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("OpenAI-Beta", "realtime=v1")
	if accountID != "" {
		headers.Set("ChatGPT-Account-Id", accountID)
	}

	s.bridgeWebSocket(c, base, headers)
}

func (s *Server) bridgeWebSocket(c *gin.Context, upstreamURL string, headers http.Header) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
		ReadBufferSize:   32 * 1024,
		WriteBufferSize:  32 * 1024,
	}
	upstreamConn, resp, err := dialer.DialContext(c.Request.Context(), upstreamURL, headers)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		status := http.StatusBadGateway
		msg := err.Error()
		if resp != nil {
			if resp.StatusCode > 0 {
				status = resp.StatusCode
			}
			if resp.Body != nil {
				if body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096)); readErr == nil && len(body) > 0 {
					msg = strings.TrimSpace(string(body))
				}
			}
		}
		c.JSON(status, gin.H{"error": gin.H{"message": msg, "type": "upstream_websocket_error"}})
		return
	}
	defer func() { _ = upstreamConn.Close() }()

	clientConn, err := voiceUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer func() { _ = clientConn.Close() }()

	errCh := make(chan error, 2)
	pump := func(dst, src *websocket.Conn) {
		for {
			msgType, payload, readErr := src.ReadMessage()
			if readErr != nil {
				errCh <- readErr
				return
			}
			if writeErr := dst.WriteMessage(msgType, payload); writeErr != nil {
				errCh <- writeErr
				return
			}
		}
	}

	go pump(upstreamConn, clientConn)
	go pump(clientConn, upstreamConn)
	<-errCh
}

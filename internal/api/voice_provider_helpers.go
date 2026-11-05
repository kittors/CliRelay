package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const (
	defaultGrokVoiceEndpoint       = "wss://grok.com/ws/app_chat/stream_audio"
	defaultClaudeVoiceEndpoint     = "wss://api.anthropic.com/api/ws/speech_to_text/voice_stream"
	defaultClaudeVoiceUserAgent    = "claude-cli/2.1.197 (external, cli)"
	defaultChatGPTLiveEndpoint     = "https://chatgpt.com/backend-api/codex/v1/live"
	defaultChatGPTSidebandEndpoint = "wss://chatgpt.com/backend-api/codex/v1/live"
	defaultOpenAIRealtimeWS        = "wss://api.openai.com/v1/realtime"
)

var grokSupportedVoices = []string{"ara", "rex", "sal", "eve", "una", "leo"}

var claudeSupportedVoices = []string{"Airy", "Mellow", "Buttery", "Gemma", "Rounded"}

var claudeSupportedVoiceModels = []string{
	"claude-sonnet-4-6",
	"claude-opus-4-7",
	"claude-haiku-4-5",
}

func resolveGrokVoiceEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("CLIRELAY_GROK_VOICE_WS_URL")); v != "" {
		return v
	}
	return defaultGrokVoiceEndpoint
}

func resolveClaudeVoiceEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("CLIRELAY_CLAUDE_VOICE_WS_URL")); v != "" {
		return v
	}
	return defaultClaudeVoiceEndpoint
}

func resolveChatGPTLiveEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("CLIRELAY_CHATGPT_LIVE_URL")); v != "" {
		return v
	}
	return defaultChatGPTLiveEndpoint
}

func resolveChatGPTSidebandEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("CLIRELAY_CHATGPT_LIVE_WS_URL")); v != "" {
		return v
	}
	return defaultChatGPTSidebandEndpoint
}

func resolveOpenAIRealtimeEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("CLIRELAY_OPENAI_REALTIME_WS_URL")); v != "" {
		return v
	}
	return defaultOpenAIRealtimeWS
}

func normalizeGrokVoice(raw string) string {
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	for _, v := range grokSupportedVoices {
		if cleaned == v {
			return v
		}
	}
	return "ara"
}

func buildGrokVoiceUpstreamURL(rawQuery url.Values, overrides map[string]string) (string, error) {
	parsed, err := url.Parse(resolveGrokVoiceEndpoint())
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	pick := func(key, fallback string) string {
		if overrides != nil {
			if v := strings.TrimSpace(overrides[key]); v != "" {
				return v
			}
		}
		if rawQuery != nil {
			if v := strings.TrimSpace(rawQuery.Get(key)); v != "" {
				return v
			}
		}
		return fallback
	}
	q.Set("model", "xai")
	q.Set("client", "oai")
	q.Set("enable_search", pick("enable_search", "true"))
	q.Set("client_name", pick("client_name", "web"))
	q.Set("voice", normalizeGrokVoice(pick("voice", "ara")))
	q.Set("enable_markdown_transcript", pick("enable_markdown_transcript", "true"))
	q.Set("x-app-language", pick("x-app-language", "en-US"))
	q.Set("x-app-timezone", pick("x-app-timezone", "America/New_York"))
	for k, vals := range rawQuery {
		if q.Get(k) == "" && len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			q.Set(k, strings.TrimSpace(vals[0]))
		}
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func buildGrokVoiceHeaders(token, cookie string) http.Header {
	headers := make(http.Header)
	headers.Set("Origin", "https://grok.com")
	headers.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	if cookie != "" {
		if strings.Contains(cookie, "=") {
			headers.Set("Cookie", cookie)
		} else {
			headers.Set("Cookie", "sso="+cookie+"; sso-rw="+cookie)
		}
	}
	if token != "" {
		if !strings.Contains(token, "=") && cookie == "" {
			headers.Set("Cookie", "sso="+token+"; sso-rw="+token)
		}
		headers.Set("Authorization", "Bearer "+token)
	}
	return headers
}

func isClaudeModelExcludedFromVoice(model string) bool {
	cleaned := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(cleaned, "fable")
}

func buildClaudeVoiceUpstreamURL(rawQuery url.Values, overrides map[string]string, extraKeyterms []string) (string, error) {
	parsed, err := url.Parse(resolveClaudeVoiceEndpoint())
	if err != nil {
		return "", err
	}
	q := url.Values{}
	pick := func(key, fallback string) string {
		if overrides != nil {
			if v := strings.TrimSpace(overrides[key]); v != "" {
				return v
			}
		}
		if rawQuery != nil {
			if v := strings.TrimSpace(rawQuery.Get(key)); v != "" {
				return v
			}
		}
		return fallback
	}
	q.Set("encoding", pick("encoding", "linear16"))
	q.Set("sample_rate", pick("sample_rate", "16000"))
	q.Set("channels", pick("channels", "1"))
	q.Set("endpointing_ms", pick("endpointing_ms", "300"))
	q.Set("utterance_end_ms", pick("utterance_end_ms", "1000"))
	q.Set("language", pick("language", "en"))

	if useConv := pick("use_conversation_engine", ""); useConv != "" {
		q.Set("use_conversation_engine", useConv)
	}
	if sttProvider := pick("stt_provider", ""); sttProvider != "" {
		q.Set("stt_provider", sttProvider)
	}

	var keyterms []string
	if rawQuery != nil {
		for _, kt := range rawQuery["keyterms"] {
			if trimmed := strings.TrimSpace(kt); trimmed != "" {
				keyterms = append(keyterms, trimmed)
			}
		}
	}
	for _, kt := range extraKeyterms {
		if trimmed := strings.TrimSpace(kt); trimmed != "" {
			keyterms = append(keyterms, trimmed)
		}
	}
	for _, kt := range keyterms {
		q.Add("keyterms", kt)
	}

	for k, vals := range rawQuery {
		if k == "keyterms" || q.Get(k) != "" {
			continue
		}
		for _, v := range vals {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				q.Add(k, trimmed)
			}
		}
	}

	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func buildClaudeVoiceHeaders(token, callerUserAgent string) http.Header {
	headers := make(http.Header)
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	headers.Set("x-app", "cli")
	ua := strings.TrimSpace(callerUserAgent)
	if !strings.HasPrefix(strings.ToLower(ua), "claude-cli/") {
		ua = defaultClaudeVoiceUserAgent
	}
	headers.Set("User-Agent", ua)
	return headers
}

func buildChatGPTLiveSidebandURL(callID string, rawQuery url.Values) (string, error) {
	base := strings.TrimRight(resolveChatGPTSidebandEndpoint(), "/")
	parsed, err := url.Parse(base + "/" + url.PathEscape(strings.TrimSpace(callID)))
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	for k, vals := range rawQuery {
		if len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			q.Set(k, strings.TrimSpace(vals[0]))
		}
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func parseChatGPTLiveOffer(r *http.Request) (sdp string, sessionJSON string, err error) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return "", "", err
	}
	defaultSession := `{"type":"realtime","model":"gpt-realtime"}`
	if qModel := strings.TrimSpace(r.URL.Query().Get("model")); qModel != "" {
		b, _ := json.Marshal(map[string]any{"type": "realtime", "model": qModel})
		defaultSession = string(b)
	}

	if strings.HasPrefix(ct, "multipart/form-data") {
		_, params, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || params["boundary"] == "" {
			return "", "", fmt.Errorf("invalid multipart boundary")
		}
		mr := multipart.NewReader(bytes.NewReader(rawBody), params["boundary"])
		for {
			part, partErr := mr.NextPart()
			if partErr == io.EOF {
				break
			}
			if partErr != nil {
				return "", "", partErr
			}
			data, _ := io.ReadAll(io.LimitReader(part, 2<<20))
			switch part.FormName() {
			case "sdp":
				sdp = strings.TrimSpace(string(data))
			case "session":
				sessionJSON = strings.TrimSpace(string(data))
			}
		}
		if sessionJSON == "" {
			sessionJSON = defaultSession
		}
		return sdp, sessionJSON, nil
	}

	if strings.HasPrefix(ct, "application/sdp") || strings.HasPrefix(ct, "text/plain") {
		return strings.TrimSpace(string(rawBody)), defaultSession, nil
	}

	trimmed := bytes.TrimSpace(rawBody)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var payload struct {
			SDP     string          `json:"sdp"`
			Offer   string          `json:"offer"`
			Model   string          `json:"model"`
			Voice   string          `json:"voice"`
			Session json.RawMessage `json:"session"`
		}
		if jsonErr := json.Unmarshal(trimmed, &payload); jsonErr != nil {
			return "", "", fmt.Errorf("invalid JSON payload")
		}
		sdp = strings.TrimSpace(payload.SDP)
		if sdp == "" {
			sdp = strings.TrimSpace(payload.Offer)
		}
		if len(payload.Session) > 0 && string(payload.Session) != "null" {
			sessionJSON = string(payload.Session)
		} else {
			sess := map[string]any{"type": "realtime", "model": "gpt-realtime"}
			if strings.TrimSpace(payload.Model) != "" {
				sess["model"] = strings.TrimSpace(payload.Model)
			}
			if strings.TrimSpace(payload.Voice) != "" {
				sess["audio"] = map[string]any{"output": map[string]any{"voice": strings.TrimSpace(payload.Voice)}}
			}
			b, _ := json.Marshal(sess)
			sessionJSON = string(b)
		}
		return sdp, sessionJSON, nil
	}

	return strings.TrimSpace(string(rawBody)), defaultSession, nil
}

func buildChatGPTLiveMultipartBody(sdp, sessionJSON string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	sdpPart, err := mw.CreateFormField("sdp")
	if err != nil {
		return nil, "", err
	}
	if _, err := sdpPart.Write([]byte(sdp)); err != nil {
		return nil, "", err
	}

	sessionPart, err := mw.CreateFormField("session")
	if err != nil {
		return nil, "", err
	}
	if _, err := sessionPart.Write([]byte(sessionJSON)); err != nil {
		return nil, "", err
	}

	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

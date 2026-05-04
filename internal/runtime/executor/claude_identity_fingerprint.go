package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeFingerprintSalt = "59cf53e54c78"

var (
	claudeServerSessionOnce sync.Once
	claudeServerSessionID   string
)

type claudeFingerprintUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

func claudeIdentityFingerprint(cfg *config.Config) (config.ClaudeIdentityFingerprintConfig, bool) {
	if cfg == nil || !cfg.IdentityFingerprint.Claude.Enabled {
		return config.ClaudeIdentityFingerprintConfig{}, false
	}
	return config.NormalizeClaudeIdentityFingerprint(cfg.IdentityFingerprint.Claude), true
}

func claudeServerStableSessionID() string {
	claudeServerSessionOnce.Do(func() {
		claudeServerSessionID = uuid.NewString()
	})
	return claudeServerSessionID
}

func claudeFingerprintSessionID(fp config.ClaudeIdentityFingerprintConfig) string {
	switch strings.TrimSpace(strings.ToLower(fp.SessionMode)) {
	case "fixed":
		if strings.TrimSpace(fp.SessionID) != "" {
			return strings.TrimSpace(fp.SessionID)
		}
		return claudeServerStableSessionID()
	case "server-stable":
		return claudeServerStableSessionID()
	default:
		return uuid.NewString()
	}
}

func applyClaudeIdentityFingerprintHeaders(headers http.Header, fp config.ClaudeIdentityFingerprintConfig, stream bool, extraBetas []string, sessionID string) {
	if headers == nil {
		return
	}
	headers.Set("Anthropic-Beta", mergeClaudeBetaHeader(fp.AnthropicBeta, extraBetas))
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	headers.Set("User-Agent", fp.UserAgent)
	headers.Set("X-App", fp.Entrypoint)
	headers.Set("X-Claude-Code-Session-Id", sessionID)
	headers.Set("X-Client-Request-Id", uuid.NewString())
	headers.Set("X-Stainless-Helper-Method", "stream")
	headers.Set("X-Stainless-Retry-Count", "0")
	headers.Set("X-Stainless-Runtime-Version", fp.StainlessRuntimeVersion)
	headers.Set("X-Stainless-Package-Version", fp.StainlessPackageVersion)
	headers.Set("X-Stainless-Runtime", "node")
	headers.Set("X-Stainless-Lang", "js")
	headers.Set("X-Stainless-Arch", mapStainlessArch())
	headers.Set("X-Stainless-Os", mapStainlessOS())
	headers.Set("X-Stainless-Timeout", fp.StainlessTimeout)
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
	for key, value := range fp.CustomHeaders {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" && !isClaudeFingerprintRuntimeBlockedHeader(key) {
			headers.Set(key, value)
		}
	}
}

func mergeClaudeBetaHeader(base string, extraBetas []string) string {
	seen := make(map[string]bool)
	var out []string
	add := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				seen[part] = true
				out = append(out, part)
			}
		}
	}
	add(base)
	for _, beta := range extraBetas {
		add(beta)
	}
	if !seen["oauth-2025-04-20"] {
		out = append([]string{"oauth-2025-04-20"}, out...)
	}
	return strings.Join(out, ",")
}

func applyClaudeIdentityFingerprintPayload(auth *cliproxyauth.Auth, payload []byte, fp config.ClaudeIdentityFingerprintConfig, sessionID string) []byte {
	payload = applyClaudeFingerprintSystem(payload, fp)
	userID, err := json.Marshal(claudeFingerprintUserID{
		DeviceID:    claudeFingerprintDeviceID(auth, fp),
		AccountUUID: claudeFingerprintAccountUUID(auth),
		SessionID:   sessionID,
	})
	if err == nil {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", string(userID))
	}
	return payload
}

func applyClaudeFingerprintSystem(payload []byte, fp config.ClaudeIdentityFingerprintConfig) []byte {
	system := gjson.GetBytes(payload, "system")
	remaining := make([]map[string]any, 0)
	appendText := func(text string, cache bool) {
		text = strings.TrimSpace(text)
		if text == "" || isClaudeFingerprintBillingText(text) || isClaudeFingerprintPrefixText(text) {
			return
		}
		block := map[string]any{"type": "text", "text": text}
		if cache {
			block["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		remaining = append(remaining, block)
	}
	if system.IsArray() {
		for _, item := range system.Array() {
			text := item.Get("text").String()
			if isClaudeFingerprintBillingText(text) || isClaudeFingerprintPrefixText(text) {
				continue
			}
			var block map[string]any
			if err := json.Unmarshal([]byte(item.Raw), &block); err == nil {
				remaining = append(remaining, block)
			}
		}
	} else if system.Type == gjson.String {
		appendText(system.String(), false)
	}

	next := []map[string]any{
		{"type": "text", "text": buildClaudeBillingHeader(payload, fp)},
		{
			"type":          "text",
			"text":          "You are Claude Code, Anthropic's official CLI for Claude.",
			"cache_control": map[string]string{"type": "ephemeral"},
		},
	}
	next = append(next, remaining...)
	raw, err := json.Marshal(next)
	if err != nil {
		return payload
	}
	payload, _ = sjson.SetRawBytes(payload, "system", raw)
	return payload
}

func buildClaudeBillingHeader(payload []byte, fp config.ClaudeIdentityFingerprintConfig) string {
	fingerprint := computeClaudeBillingFingerprint(firstClaudeUserMessageText(payload), fp.CLIVersion)
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s;", fp.CLIVersion, fingerprint, fp.Entrypoint)
}

func firstClaudeUserMessageText(payload []byte) string {
	messages := gjson.GetBytes(payload, "messages")
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			return content.String()
		}
		if content.IsArray() {
			for _, block := range content.Array() {
				if block.Get("type").String() == "text" {
					return block.Get("text").String()
				}
			}
		}
	}
	return ""
}

func computeClaudeBillingFingerprint(messageText, version string) string {
	chars := []rune(messageText)
	pick := func(index int) rune {
		if index >= 0 && index < len(chars) {
			return chars[index]
		}
		return '0'
	}
	input := fmt.Sprintf("%s%c%c%c%s", claudeFingerprintSalt, pick(4), pick(7), pick(20), version)
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:3]
}

func claudeFingerprintDeviceID(auth *cliproxyauth.Auth, fp config.ClaudeIdentityFingerprintConfig) string {
	if isClaudeFingerprintDeviceID(fp.DeviceID) {
		return strings.ToLower(fp.DeviceID)
	}
	seed := claudeFingerprintAccountUUID(auth)
	if seed == "" && auth != nil {
		seed = auth.ID
	}
	if seed == "" {
		seed = "claude-oauth"
	}
	sum := sha256.Sum256([]byte("claude-device:" + seed))
	return hex.EncodeToString(sum[:])
}

func claudeFingerprintAccountUUID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"account_uuid", "account_id", "uuid"} {
		if auth.Metadata != nil {
			if value, ok := auth.Metadata[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return value
			}
		}
	}
	return auth.ID
}

func isClaudeFingerprintDeviceID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' {
			continue
		}
		return false
	}
	return true
}

func isClaudeFingerprintBillingText(text string) bool {
	return strings.Contains(text, "x-anthropic-billing-header")
}

func isClaudeFingerprintPrefixText(text string) bool {
	return strings.Contains(text, "You are Claude Code")
}

func isClaudeFingerprintRuntimeBlockedHeader(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(key, "x-stainless-") {
		return true
	}
	switch key {
	case "authorization", "content-type", "accept", "connection", "x-api-key",
		"anthropic-beta", "anthropic-version", "anthropic-dangerous-direct-browser-access",
		"user-agent", "x-app", "x-client-request-id", "x-claude-code-session-id":
		return true
	default:
		return false
	}
}

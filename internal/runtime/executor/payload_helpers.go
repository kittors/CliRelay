package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/payloadrules"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// applyPayloadConfigWithRoot applies the payload rules of cfg to payload. Rule
// paths are relative to root ("request" for Gemini CLI), entries restricted to
// another protocol do not match, and defaults are checked against original
// when provided. requestedModel carries the client-visible model name before
// alias resolution so payload rules can target aliases precisely. The rules
// themselves live in internal/payloadrules, which the management dry-run runs
// too.
func applyPayloadConfigWithRoot(cfg *config.Config, model, protocol, root string, payload, original []byte, requestedModel string) []byte {
	if cfg == nil {
		return payload
	}
	return payloadrules.Apply(cfg.Payload, payloadrules.Request{
		Model:          model,
		RequestedModel: requestedModel,
		Protocol:       protocol,
		Root:           root,
		Body:           payload,
		Original:       original,
	})
}

func payloadRequestedModel(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return strings.TrimSpace(v)
	case []byte:
		if len(v) == 0 {
			return fallback
		}
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return fallback
		}
		return trimmed
	default:
		return fallback
	}
}

// extractSystemMessagesAsInstructions extracts all system-role messages from an
// OpenAI chat-completions format JSON payload and concatenates their content with
// newlines. This is used by the Codex executor to preserve system prompts
// (including those injected by SystemPromptMiddleware) when translating from
// OpenAI messages format to Codex Responses API instructions format.
//
// Returns an empty string if there are no system messages.
func extractSystemMessagesAsInstructions(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var parts []string
	messages.ForEach(func(_, msg gjson.Result) bool {
		if strings.EqualFold(msg.Get("role").String(), "system") {
			content := strings.TrimSpace(msg.Get("content").String())
			if content != "" {
				parts = append(parts, content)
			}
		}
		return true
	})
	return strings.Join(parts, "\n")
}

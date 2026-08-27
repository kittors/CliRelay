package management

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Model connectivity test.
//
// The panel used to answer "can this model be reached" from the browser: it
// listed the tenant's API keys, picked one itself, and sent a chat completion to
// the public /v1 endpoint. That answers a different question — whether that
// particular business identity may use the model. An operator checking a healthy
// kimi account got "no auth available" because the key it happened to pick was
// bound to an end user restricted to one channel group, and no amount of channel
// or model configuration on the account side could change that.
//
// Image and video generation already test through the auth manager for exactly
// this reason. This does the same for chat models: management authority, tenant
// scope, no API key in the loop.

const modelTestPromptLimit = 4000

// modelTestTimeout bounds one probe. Long enough for a cold upstream, short
// enough that an operator is not left watching a spinner.
const modelTestTimeout = 120 * time.Second

type modelTestRequest struct {
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Channel string `json:"channel"`
}

// PostModelTest runs one non-streaming completion against a model using
// management authority, and reports what came back.
func (h *Handler) PostModelTest(c *gin.Context) {
	var body modelTestRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model is required"})
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is required"})
		return
	}
	if len(prompt) > modelTestPromptLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "prompt is too long"})
		return
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}

	providers := sdkmodelcatalog.GetProviderName(model)
	if len(providers) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no provider serves model %q", model)})
		return
	}

	payload, err := buildModelTestPayload(model, prompt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), modelTestTimeout)
	defer cancel()

	startedAt := time.Now()
	resp, execErr := h.authManager.Execute(ctx, providers, coreexecutor.Request{
		Model:   model,
		Payload: payload,
		Format:  sdktranslator.FromString("openai"),
	}, coreexecutor.Options{
		OriginalRequest: payload,
		SourceFormat:    sdktranslator.FromString("openai"),
		Metadata:        modelTestMetadata(effectiveTenantID(c), body.Channel),
	})
	elapsed := time.Since(startedAt).Milliseconds()

	if execErr != nil {
		// The upstream reason is the whole point of the probe, so it is reported
		// as a result rather than swallowed into a 5xx.
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": execErr.Error(), "duration_ms": elapsed})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"content":     modelTestContent(resp.Payload),
		"duration_ms": elapsed,
	})
}

// modelTestMetadata scopes the probe to the tenant and, when the operator picked
// a channel, to that channel. Restricting by channel is what the panel's channel
// selector always implied but never did: it only used the value to choose an API
// key, so the request could still land on a different account.
func modelTestMetadata(tenantID, channel string) map[string]any {
	meta := map[string]any{
		coreexecutor.SinglePickMetadataKey: true,
		coreexecutor.TenantMetadataKey:     coreauth.NormalizedTenantID(tenantID),
	}
	if channel = strings.TrimSpace(channel); channel != "" {
		meta["allowed-channels"] = channel
	}
	return meta
}

func buildModelTestPayload(model, prompt string) ([]byte, error) {
	payload := []byte(`{"stream":false,"messages":[]}`)
	var err error
	if payload, err = sjson.SetBytes(payload, "model", model); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "messages.0.role", "user"); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "messages.0.content", prompt); err != nil {
		return nil, fmt.Errorf("build model test payload: %w", err)
	}
	return payload, nil
}

// modelTestContent pulls the assistant text out of an OpenAI-shaped response,
// falling back to the raw body so an unexpected shape is still inspectable.
func modelTestContent(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if text := gjson.GetBytes(payload, "choices.0.message.content").String(); strings.TrimSpace(text) != "" {
		return text
	}
	if parts := gjson.GetBytes(payload, "choices.0.message.content.#.text"); parts.Exists() {
		texts := make([]string, 0, len(parts.Array()))
		for _, part := range parts.Array() {
			if value := strings.TrimSpace(part.String()); value != "" {
				texts = append(texts, value)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return string(payload)
}

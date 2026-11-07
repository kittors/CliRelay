package executor

import (
	"context"
	"io"
	"testing"

	chat "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/antigravity/openai/chat-completions"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// A Chat Completions request carrying an MCP tool whose array argument declares no items,
// through the same translator and executor steps as production. Antigravity refused such
// requests outright ("...parameters.properties[machineIds].items: missing field.") because
// the executor sends `parameters`, which requires items on every array.
func TestAntigravityBuildRequest_FillsMissingArrayItems(t *testing.T) {
	openAI := []byte(`{
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {
			"name": "batch_run",
			"description": "Run commands on machines.",
			"parameters": {"type": "object", "properties": {
				"machineIds": {"type": "array", "description": "Target machines"},
				"tags": {"type": "array", "items": {"type": "string"}}
			}, "required": ["machineIds"]}
		}}]
	}`)
	// One model per cleaning mode: Gemini, and Claude's VALIDATED schema.
	for _, model := range []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"} {
		t.Run(model, func(t *testing.T) {
			payload := chat.ConvertOpenAIRequestToAntigravity(model, openAI, false)
			req, _, err := (&AntigravityExecutor{}).buildRequest(context.Background(), &cliproxyauth.Auth{}, "token", model, payload, false, "", "https://example.com", 0)
			if err != nil {
				t.Fatalf("buildRequest error: %v", err)
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body error: %v", err)
			}

			decl := gjson.GetBytes(raw, "request.tools.0.functionDeclarations.0")
			if decl.Get("parametersJsonSchema").Exists() || !decl.Get("parameters").Exists() {
				t.Fatalf("expected the schema under parameters: %s", decl.Raw)
			}
			if items := decl.Get("parameters.properties.machineIds.items"); !items.IsObject() || len(items.Map()) != 0 {
				t.Fatalf("machineIds.items = %q, want {}; declaration: %s", items.Raw, decl.Raw)
			}
			if got := decl.Get("parameters.properties.tags.items.type").String(); got != "string" {
				t.Fatalf("declared tags.items lost: %s", decl.Raw)
			}
		})
	}
}

package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ---------------------------------------------------------------------------
// Unit: opencodeGoInjectCodexToolBridgeTools
// ---------------------------------------------------------------------------

func TestInjectCodexToolBridgeTools_AddsWhenMissing(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"existing_tool"}}]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	tools := gjson.GetBytes(got, "tools").Array()
	foundExisting := false
	foundComputerUse := false
	foundNodeRepl := false
	for _, tool := range tools {
		name := tool.Get("function.name").String()
		if name == "existing_tool" {
			foundExisting = true
		}
		if name == "mcp__computer_use__click" {
			foundComputerUse = true
		}
		if name == "mcp__node_repl__js" {
			foundNodeRepl = true
		}
	}
	if !foundExisting {
		t.Error("original tool 'existing_tool' was removed")
	}
	if !foundComputerUse {
		t.Error("mcp__computer_use__click was not injected")
	}
	if !foundNodeRepl {
		t.Error("mcp__node_repl__js was not injected")
	}
	if len(tools) != 12 {
		t.Errorf("expected 12 tools (1 existing + 10 CU + node_repl js), got %d", len(tools))
	}
}

func TestInjectCodexToolBridgeTools_DoesNotDuplicateExistingFunctions(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"mcp__computer_use__click"}},{"type":"function","function":{"name":"mcp__node_repl__js"}}]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	clickCount := 0
	nodeReplCount := 0
	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		switch tool.Get("function.name").String() {
		case "mcp__computer_use__click":
			clickCount++
		case "mcp__node_repl__js":
			nodeReplCount++
		}
	}
	if clickCount != 1 {
		t.Errorf("expected one mcp__computer_use__click, got %d", clickCount)
	}
	if nodeReplCount != 1 {
		t.Errorf("expected one mcp__node_repl__js, got %d", nodeReplCount)
	}
}

func TestInjectCodexToolBridgeTools_SkipsNoTools(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	if gjson.GetBytes(got, "tools").Exists() {
		t.Error("should not add tools array when none existed")
	}
}

func TestInjectCodexToolBridgeTools_EmptyTools(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	tools := gjson.GetBytes(got, "tools").Array()
	if len(tools) != 0 {
		t.Errorf("expected 0 tools for empty array, got %d", len(tools))
	}
}

func TestInjectCodexToolBridgeTools_InvalidJSON(t *testing.T) {
	got := opencodeGoInjectCodexToolBridgeTools([]byte(`not json`))
	if string(got) != "not json" {
		t.Error("should return original payload unchanged for invalid JSON")
	}
}

func TestInjectCodexToolBridgeTools_NilPayload(t *testing.T) {
	got := opencodeGoInjectCodexToolBridgeTools(nil)
	if got != nil {
		t.Error("should return nil for nil payload")
	}
}

func TestInjectCodexToolBridgeTools_ExpandsNamespaceTools(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"namespace","name":"mcp__computer_use__"}]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	tools := gjson.GetBytes(got, "tools").Array()
	foundComputerUse := false
	foundNodeRepl := false
	for _, tool := range tools {
		switch tool.Get("name").String() {
		case "mcp__computer_use__click":
			foundComputerUse = true
		case "mcp__node_repl__js":
			foundNodeRepl = true
		}
	}
	if !foundComputerUse {
		t.Errorf("namespace tool was not expanded to mcp__computer_use__click; body=%s", string(got))
	}
	if !foundNodeRepl {
		t.Errorf("namespace tool did not receive mcp__node_repl__js bridge; body=%s", string(got))
	}
}

func TestInjectCodexToolBridgeTools_PreservesModelAndMessages(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"test"}],"tools":[{"type":"function","function":{"name":"foo"}}]}`)
	got := opencodeGoInjectCodexToolBridgeTools(payload)

	if model := gjson.GetBytes(got, "model").String(); model != "deepseek-v4-flash" {
		t.Errorf("model changed, got %q", model)
	}
	if msg := gjson.GetBytes(got, "messages.0.content").String(); msg != "test" {
		t.Errorf("messages changed, got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// Verify all 10 Computer Use functions are well-formed
// ---------------------------------------------------------------------------

func TestComputerUseFunctions_AllValidDefinitions(t *testing.T) {
	for i, fn := range mcpComputerUseFunctions {
		fnMap, ok := fn["function"].(map[string]any)
		if !ok {
			t.Fatalf("function %d missing 'function' key", i)
		}
		name, _ := fnMap["name"].(string)
		if !hasPrefix(name, "mcp__computer_use__") {
			t.Errorf("function %d has unexpected name %q", i, name)
		}
		// Verify it can be serialized
		_, err := json.Marshal(fn)
		if err != nil {
			t.Fatalf("function %d (%s) failed to marshal: %v", i, name, err)
		}
		// Verify sjson.SetBytes works with it
		testPayload := []byte(`{"tools":[]}`)
		_, err = sjson.SetBytes(testPayload, "tools.0", fn)
		if err != nil {
			t.Fatalf("function %d (%s) failed sjson roundtrip: %v", i, name, err)
		}
	}
}

func TestComputerUseFunctions_Count(t *testing.T) {
	if len(mcpComputerUseFunctions) != 10 {
		t.Fatalf("expected 10 Computer Use functions, got %d", len(mcpComputerUseFunctions))
	}

	expectedNames := []string{
		"mcp__computer_use__click",
		"mcp__computer_use__drag",
		"mcp__computer_use__get_app_state",
		"mcp__computer_use__list_apps",
		"mcp__computer_use__perform_secondary_action",
		"mcp__computer_use__press_key",
		"mcp__computer_use__scroll",
		"mcp__computer_use__select_text",
		"mcp__computer_use__set_value",
		"mcp__computer_use__type_text",
	}
	for _, expected := range expectedNames {
		found := false
		for _, fn := range mcpComputerUseFunctions {
			fnMap, _ := fn["function"].(map[string]any)
			if fnMap["name"].(string) == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected function %q not found", expected)
		}
	}
}

func TestNodeReplJSFunction_ValidDefinition(t *testing.T) {
	name, _, params, ok := opencodeGoBridgeFunctionParts(mcpNodeReplJSFunction)
	if !ok {
		t.Fatal("mcpNodeReplJSFunction is not a valid bridge function")
	}
	if name != "mcp__node_repl__js" {
		t.Fatalf("unexpected node repl function name %q", name)
	}
	if required := gjson.Get(mustMarshalJSON(t, params), "required.0").String(); required != "code" {
		t.Fatalf("expected node repl js to require code, got %q", required)
	}
}

// ---------------------------------------------------------------------------
// Integration: Execute with Codex tool bridge injection
// ---------------------------------------------------------------------------

func TestOpenCodeGoExecutorInjectsCodexToolBridgeTools(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_cu","object":"chat.completion","created":1,"model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	oldURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"use computer"}],"tools":[{"type":"function","function":{"name":"existing_tool"}}]}`)

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Verify upstream body has Codex bridge tools injected
	tools := gjson.GetBytes(gotBody, "tools").Array()
	hasCU := false
	hasNodeRepl := false
	hasExisting := false
	for _, tool := range tools {
		name := tool.Get("function.name").String()
		if name == "mcp__computer_use__get_app_state" {
			hasCU = true
		}
		if name == "mcp__node_repl__js" {
			hasNodeRepl = true
		}
		if name == "existing_tool" {
			hasExisting = true
		}
	}
	if !hasExisting {
		t.Error("upstream body missing existing_tool")
	}
	if !hasCU {
		t.Errorf("upstream body missing mcp__computer_use__ tools; body=%s", string(gotBody))
	}
	if !hasNodeRepl {
		t.Errorf("upstream body missing mcp__node_repl__js; body=%s", string(gotBody))
	}
}

func TestOpenCodeGoExecutorInjectsBridgeForNonDeepSeekOpenCodeGoModel(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_no","object":"chat.completion","created":1,"model":"gpt-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	oldURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"existing_tool"}}]}`)

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// Non-DeepSeek OpenCode-Go models should still get the generic Codex bridge.
	tools := gjson.GetBytes(gotBody, "tools").Array()
	hasCU := false
	hasNodeRepl := false
	for _, tool := range tools {
		name := tool.Get("function.name").String()
		if hasPrefix(name, "mcp__computer_use__") {
			hasCU = true
		}
		if name == "mcp__node_repl__js" {
			hasNodeRepl = true
		}
	}
	if !hasCU {
		t.Errorf("non-deepseek opencode-go model should get Computer Use bridge tools; body=%s", string(gotBody))
	}
	if !hasNodeRepl {
		t.Errorf("non-deepseek opencode-go model should get node_repl js bridge; body=%s", string(gotBody))
	}
}

func TestOpenAICompatExecutorInjectsCodexToolBridgeTools(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_bridge","object":"chat.completion","created":1,"model":"third-party-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test-key",
	}}
	payload := []byte(`{"model":"third-party-model","messages":[{"role":"user","content":"use browser"}],"tools":[{"type":"function","function":{"name":"existing_tool"}}]}`)

	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "third-party-model",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	hasCU := false
	hasNodeRepl := false
	for _, tool := range gjson.GetBytes(gotBody, "tools").Array() {
		name := tool.Get("function.name").String()
		if hasPrefix(name, "mcp__computer_use__") {
			hasCU = true
		}
		if name == "mcp__node_repl__js" {
			hasNodeRepl = true
		}
	}
	if !hasCU {
		t.Errorf("openai-compatible executor should get Computer Use bridge tools; body=%s", string(gotBody))
	}
	if !hasNodeRepl {
		t.Errorf("openai-compatible executor should get node_repl js bridge; body=%s", string(gotBody))
	}
}

// ---------------------------------------------------------------------------
// Integration: ExecuteStream with Codex tool bridge injection
// ---------------------------------------------------------------------------

func TestOpenCodeGoExecutorStreamInjectsCodexToolBridgeTools(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chunk1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	oldURL := opencodeGoBaseURL
	opencodeGoBaseURL = server.URL + "/v1"
	t.Cleanup(func() { opencodeGoBaseURL = oldURL })

	exec := NewOpenCodeGoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	payload := []byte(`{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"use computer"}],"tools":[{"type":"function","function":{"name":"tool1"}}]}`)

	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	// Verify upstream body has Codex bridge tools
	hasCU := false
	hasNodeRepl := false
	for _, tool := range gjson.GetBytes(gotBody, "tools").Array() {
		name := tool.Get("function.name").String()
		if hasPrefix(name, "mcp__computer_use__") {
			hasCU = true
		}
		if name == "mcp__node_repl__js" {
			hasNodeRepl = true
		}
	}
	if !hasCU {
		t.Errorf("stream request missing mcp__computer_use__ tools; body=%s", string(gotBody))
	}
	if !hasNodeRepl {
		t.Errorf("stream request missing mcp__node_repl__js; body=%s", string(gotBody))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func mustMarshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return string(b)
}

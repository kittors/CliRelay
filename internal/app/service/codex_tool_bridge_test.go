package serviceapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// offeredToolsThroughSynthesizedCompatEntry runs one chat request declaring
// tool_a through the path a configured openai-compatibility entry takes in
// production: config synthesizer, executor registry, then the bound executor.
// It returns the tool names the upstream was offered.
func offeredToolsThroughSynthesizedCompatEntry(t *testing.T, entry config.OpenAICompatibility) []string {
	t.Helper()
	bodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_bridge","object":"chat.completion","created":1,"model":"tool-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	entry.Name = "My-Compat"
	entry.BaseURL = upstream.URL + "/v1"
	entry.APIKeyEntries = []config.OpenAICompatibilityAPIKey{{APIKey: "test-key"}}
	entry.Models = []config.OpenAICompatibilityModel{{Name: "tool-model"}}
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{entry}}

	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil || len(auths) != 1 {
		t.Fatalf("Synthesize: %d auths, err=%v", len(auths), err)
	}
	auth := auths[0]
	manager := coreauth.NewManager(nil, nil, nil)
	RegisterExecutorForAuth(manager, cfg, auth, false, nil)
	bound, ok := manager.Executor(auth.Provider)
	if !ok || bound == nil {
		t.Fatalf("no executor bound for %q", auth.Provider)
	}

	payload := []byte(`{"model":"tool-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"tool_a","description":"t","parameters":{"type":"object","properties":{}}}}]}`)
	if _, err := bound.Execute(context.Background(), auth, coreexecutor.Request{
		Model: "tool-model", Payload: payload, Format: sdktranslator.FormatOpenAI,
	}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var forwarded []byte
	select {
	case forwarded = <-bodies:
	default:
		t.Fatal("upstream received no request")
	}
	names := []string{}
	for _, tool := range gjson.GetBytes(forwarded, "tools").Array() {
		names = append(names, tool.Get("function.name").String())
	}
	return names
}

// Regression for #1041: an openai-compatibility entry that says nothing about
// the Codex tool bridge forwards the caller's tools unchanged.
func TestSynthesizedCompatEntryOffersOnlyCallerTools(t *testing.T) {
	got := offeredToolsThroughSynthesizedCompatEntry(t, config.OpenAICompatibility{})
	if want := []string{"tool_a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream was offered %d tools %v, want only the caller's %v", len(got), got, want)
	}
}

// Operators who serve Codex Desktop through a compatible upstream turn the
// bridge on per entry, and the setting reaches the executor end to end.
func TestSynthesizedCompatEntryBridgesCodexToolsWhenOptedIn(t *testing.T) {
	got := offeredToolsThroughSynthesizedCompatEntry(t, config.OpenAICompatibility{CodexToolBridge: true})
	offered := map[string]bool{}
	for _, name := range got {
		offered[name] = true
	}
	// tool_a plus ten Computer Use functions and mcp__node_repl__js.
	if len(got) != 12 || got[0] != "tool_a" || !offered["mcp__computer_use__get_app_state"] || !offered["mcp__node_repl__js"] {
		t.Fatalf("upstream was offered %d tools %v, want tool_a followed by the Codex tool bridge", len(got), got)
	}
}

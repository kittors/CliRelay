package contextretrieval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestReduceResponsesInputUsesFTSMatchesAndRecentItems(t *testing.T) {
	oldRelevant := strings.Repeat("pkg/router/session.go websocket idle timeout ", 15)
	oldIrrelevant := strings.Repeat("unrelated billing invoice report ", 120)
	latest := "Please fix websocket idle timeout in pkg/router/session.go"
	raw := []byte(`{"model":"gpt-5.3-codex","instructions":"keep instructions","input":[` +
		`{"role":"user","content":"` + oldRelevant + `"},` +
		`{"role":"user","content":"` + oldIrrelevant + `"},` +
		`{"role":"user","content":"` + latest + `"}` +
		`]}`)

	reduced, report, err := Reduce(context.Background(), raw, "gpt-5.3-codex", "openai-response", config.ContextRetrievalConfig{
		Enabled:             true,
		MaxInputBytes:       2000,
		PreserveRecentTurns: 1,
		Chunk:               config.ContextRetrievalChunkConfig{MaxBytes: 512},
		Retrieval:           config.ContextRetrievalSearchConfig{TopK: 1},
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("expected reduction to apply")
	}
	if len(reduced) >= len(raw) {
		t.Fatalf("expected reduced payload to shrink: %d >= %d", len(reduced), len(raw))
	}
	input := gjson.GetBytes(reduced, "input")
	if !input.IsArray() {
		t.Fatalf("input is not array: %s", input.Raw)
	}
	if got := len(input.Array()); got != 2 {
		t.Fatalf("input len = %d, want 2; body=%s", got, reduced)
	}
	body := string(reduced)
	if !strings.Contains(body, "pkg/router/session.go") {
		t.Fatalf("expected relevant old context to be retained: %s", body)
	}
	if strings.Contains(body, "billing invoice") {
		t.Fatalf("expected irrelevant context to be removed: %s", body)
	}
	if got := gjson.GetBytes(reduced, "instructions").String(); got != "keep instructions" {
		t.Fatalf("instructions = %q", got)
	}
}

func TestReduceSkipsUnmatchedModel(t *testing.T) {
	raw := []byte(`{"model":"other","input":[{"role":"user","content":"` + strings.Repeat("x", 2000) + `"}]}`)
	reduced, report, err := Reduce(context.Background(), raw, "other", "openai-response", config.ContextRetrievalConfig{
		Enabled:       true,
		Models:        []config.PayloadModelRule{{Name: "gpt-5.3-codex"}},
		MaxInputBytes: 100,
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if report.Applied {
		t.Fatal("expected no reduction")
	}
	if string(reduced) != string(raw) {
		t.Fatal("expected original payload")
	}
}

func TestReduceChatMessagesPreservesSystemAndLatest(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.3-codex","messages":[` +
		`{"role":"system","content":"system rules"},` +
		`{"role":"user","content":"` + strings.Repeat("old noise ", 200) + `"},` +
		`{"role":"user","content":"latest question about compile error FooBar"}` +
		`]}`)
	reduced, report, err := Reduce(context.Background(), raw, "gpt-5.3-codex", "openai", config.ContextRetrievalConfig{
		Enabled:             true,
		MaxInputBytes:       700,
		PreserveRecentTurns: 1,
		Retrieval:           config.ContextRetrievalSearchConfig{TopK: 1},
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("expected reduction to apply")
	}
	var decoded struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(reduced, &decoded); err != nil {
		t.Fatalf("unmarshal reduced: %v", err)
	}
	if len(decoded.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2; body=%s", len(decoded.Messages), reduced)
	}
	if decoded.Messages[0]["role"] != "system" {
		t.Fatalf("first role = %v, want system", decoded.Messages[0]["role"])
	}
}

func TestReduceCodexAwareInsertsSummaryAndPreservesToolPair(t *testing.T) {
	noise := strings.Repeat("irrelevant historical terminal output ", 80)
	raw := []byte(`{"model":"gpt-5.3-codex","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"old notes mention internal/router/session.go and command go test ./... failed with timeout"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"archived signal: failed migration in internal/db/migrate.go"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"` + noise + `"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"internal/router/session.go\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"panic: websocket timeout in internal/router/session.go"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"latest asks about websocket timeout"}]}` +
		`]}`)

	reduced, report, err := Reduce(context.Background(), raw, "gpt-5.3-codex", "openai-response", config.ContextRetrievalConfig{
		Enabled:             true,
		MaxInputBytes:       1200,
		PreserveRecentTurns: 1,
		Retrieval:           config.ContextRetrievalSearchConfig{TopK: 1},
		CodexAware: config.CodexAwareContextConfig{
			Enabled:              true,
			PreserveToolPairs:    true,
			InsertSummary:        true,
			MaxSummaryBytes:      800,
			PreserveRecentErrors: 1,
		},
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("expected reduction to apply")
	}
	body := string(reduced)
	if !strings.Contains(body, "Retrieved summary") {
		t.Fatalf("expected synthetic summary, got %s", body)
	}
	if !strings.Contains(body, "internal/router/session.go") {
		t.Fatalf("expected file path in retained context or summary, got %s", body)
	}
	if strings.Contains(body, "call_1") && (!strings.Contains(body, "function_call") || !strings.Contains(body, "function_call_output")) {
		t.Fatalf("expected tool call/output pair to stay together, got %s", body)
	}
}

func TestReduceCodexAwareTrimsToolPairAtomically(t *testing.T) {
	largeOutput := strings.Repeat("terminal output mentions pkg/router/session.go ", 120)
	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5.3-codex",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "old notes mention pkg/router/session.go"}}},
			{"type": "function_call", "call_id": "call_atomic", "name": "read_file", "arguments": `{"path":"pkg/router/session.go"}`},
			{"type": "function_call_output", "call_id": "call_atomic", "output": largeOutput},
			{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "latest asks about pkg/router/session.go timeout"}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}

	reduced, report, err := Reduce(context.Background(), raw, "gpt-5.3-codex", "openai-response", config.ContextRetrievalConfig{
		Enabled:             true,
		MaxInputBytes:       900,
		PreserveRecentTurns: 1,
		Retrieval:           config.ContextRetrievalSearchConfig{TopK: 2},
		CodexAware: config.CodexAwareContextConfig{
			Enabled:           true,
			PreserveToolPairs: true,
			InsertSummary:     true,
		},
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("expected reduction to apply")
	}
	body := string(reduced)
	if strings.Contains(body, "call_atomic") && (!strings.Contains(body, "function_call") || !strings.Contains(body, "function_call_output")) {
		t.Fatalf("tool pair was split by trimming: %s", body)
	}
}

func TestReduceCodexAwareDropsOrphanFunctionCall(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5.3-codex",
		"input": []map[string]any{
			{"type": "function_call", "call_id": "call_orphan", "name": "shell_command", "arguments": `{"cmd":"go test ./..."}`},
			{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": strings.Repeat("latest compile failure ", 120)}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal raw: %v", err)
	}

	reduced, report, err := Reduce(context.Background(), raw, "gpt-5.3-codex", "openai-response", config.ContextRetrievalConfig{
		Enabled:             true,
		MaxInputBytes:       700,
		PreserveRecentTurns: 2,
		CodexAware: config.CodexAwareContextConfig{
			Enabled:           true,
			PreserveToolPairs: true,
			ToolPairRepair:    "drop-orphans",
		},
	})
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if !report.Applied {
		t.Fatal("expected reduction to apply")
	}
	if strings.Contains(string(reduced), "call_orphan") {
		t.Fatalf("orphan function_call was not dropped: %s", reduced)
	}
}

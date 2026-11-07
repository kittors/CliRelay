package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"math/rand"
	"strings"
	"testing"
)

func prefixHistoryFixtureForTest(calls int, textBytes int) []byte {
	tools := []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object"}}, map[string]any{"type": "web_search_20250305", "name": "web_search"}}
	messages := make([]any, 0, calls*2)
	for i := 0; i < calls; i++ {
		messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "name": "read_file", "id": fmt.Sprint("id", i), "input": map[string]any{"path": "synthetic.txt"}}}})
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprint("id", i), "content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", textBytes)}, map[string]any{"type": "tool_reference", "tool_name": "read_file"}}}}})
	}
	body, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": messages, "tools": tools, "tool_choice": map[string]any{"type": "tool", "name": "read_file"}, "unknown": map[string]any{"preserve": "opaque"}})
	return body
}

func TestClaudeToolPrefixBoundedRequestCopies(t *testing.T) {
	body := prefixHistoryFixtureForTest(64, 4096)
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = applyClaudeToolPrefix(body, "proxy_")
		}
	})
	if allocated := result.AllocedBytesPerOp(); allocated > int64(len(body))*8 {
		t.Fatalf("tool prefix allocates %d bytes for %d-byte request; repeated full-request copies exceed eight request sizes", allocated, len(body))
	}
}

func TestClaudeToolPrefixExactLegacyBytes(t *testing.T) {
	fixtures := [][]byte{
		prefixHistoryFixtureForTest(100, 128),
		[]byte(` { "messages": [{"content":[{"type":"tool_reference","tool_name":"quote\"<é\\"},{"type":"tool_use","name":"read_file","input":{"name":"untouched"}}]}], "tools":[{"name":"read_file"},{"name":"web_search","type":"web_search_20250305"}],"unknown":1e+03 } `),
		[]byte(`{"tools":[{"name":123},{"name":true},{"name":null},{"name":"proxy_done"}],"messages":[]}`),
		[]byte(`{"tools":[{"name":"same","name":"other"}],"messages":[{"content":[{"type":"tool_use","name":"same"}]}],"tools":[{"name":"ignored"}]}`),
		[]byte(`{"messages":[{"content":[{"type":"tool_use","name":"web_search"},{"type":"tool_use","name":"special"},{"type":"tool_reference","tool_name":"special"}]}],"tools":[{"name":"special","type":"custom_builtin"},{"name":"special"}]}`),
		[]byte(`{"tools":[{"name":"x"}],"messages":[{"content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"x"}]}]}]}`),
		[]byte(`{"tools":[{"name":"x"}],"messages":[`),
	}
	for i, body := range fixtures {
		for _, prefix := range []string{"", "proxy_", "p\"<é\\"} {
			original := bytes.Clone(body)
			want := baselineToolPrefixForTest(body, prefix)
			got := applyClaudeToolPrefix(body, prefix)
			if !bytes.Equal(got, want) {
				t.Fatalf("fixture %d prefix %q differs from legacy", i, prefix)
			}
			if !bytes.Equal(body, original) {
				t.Fatalf("fixture %d mutated input", i)
			}
		}
	}
}

func BenchmarkClaudeToolPrefixHistory(b *testing.B) {
	for _, calls := range []int{1, 32, 128, 512} {
		body := prefixHistoryFixtureForTest(calls, 4096)
		b.Run(fmt.Sprint(calls), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				_ = applyClaudeToolPrefix(body, "proxy_")
			}
		})
	}
}

func baselineToolPrefixForTest(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	builtinTools := map[string]bool{}
	for _, name := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		builtinTools[name] = true
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func TestClaudeToolPrefixGeneratedLegacyParity(t *testing.T) {
	rng := rand.New(rand.NewSource(34))
	names := []any{"read_file", "proxy_done", "web_search", "special", "x\"<é\\", 123, true, nil, ""}
	for trial := 0; trial < 200; trial++ {
		var tools, messages []any
		for i := 0; i < rng.Intn(24); i++ {
			tool := map[string]any{"name": names[rng.Intn(len(names))], "unknown": map[string]any{"name": "preserve"}}
			if rng.Intn(4) == 0 {
				tool["type"] = "custom_builtin"
			}
			tools = append(tools, tool)
		}
		for i := 0; i < rng.Intn(12); i++ {
			var content []any
			for j := 0; j < rng.Intn(12); j++ {
				name := names[rng.Intn(len(names))]
				kind := rng.Intn(4)
				var part any
				switch kind {
				case 0:
					part = map[string]any{"type": "tool_use", "name": name, "input": map[string]any{"name": "preserve", "text": "{}[]\""}}
				case 1:
					part = map[string]any{"type": "tool_reference", "tool_name": name}
				case 2:
					part = map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "tool_reference", "tool_name": name}, map[string]any{"type": "text", "text": "preserve"}}}
				default:
					part = map[string]any{"type": "text", "text": "untouched"}
				}
				content = append(content, part)
			}
			messages = append(messages, map[string]any{"content": content})
		}
		body, _ := json.Marshal(map[string]any{"tools": tools, "messages": messages, "tool_choice": map[string]any{"type": "tool", "name": names[rng.Intn(len(names))]}})
		original := bytes.Clone(body)
		for _, prefix := range []string{"proxy_", "p\"<é\\", ""} {
			want := baselineToolPrefixForTest(body, prefix)
			got := applyClaudeToolPrefix(body, prefix)
			if !bytes.Equal(want, got) {
				t.Fatalf("trial %d prefix %q differs", trial, prefix)
			}
		}
		if !bytes.Equal(body, original) {
			t.Fatalf("trial %d mutated input", trial)
		}
	}
}

package executor

import (
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// BenchmarkAntigravityBuildRequest measures buildRequest on the shape that made it
// the relay's largest allocator in production (60% of a 21-minute heap profile on
// 2026-11-19): a multi-megabyte conversation carrying screenshots, plus forty tool
// declarations whose raw schemas still need renaming and cleaning.
//
//	go test -run '^$' -bench BenchmarkAntigravityBuildRequest -benchmem ./internal/runtime/executor/
func BenchmarkAntigravityBuildRequest(b *testing.B) {
	payload := antigravityBenchPayload(40, 4, 1<<20)
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"project_id": "bench-project"}}
	for _, model := range []string{"claude-sonnet-4-5", "gemini-2.5-pro"} {
		b.Run(model, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(len(payload)), "payload-bytes")
			for i := 0; i < b.N; i++ {
				if _, _, err := executor.buildRequest(context.Background(), auth, "token", model, payload, true, "", "https://example.com", 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// antigravityBenchPayload builds a Gemini-shaped request with the given number of
// tools, screenshots and screenshot size, from a fixed seed so runs compare.
func antigravityBenchPayload(tools, screenshots, screenshotBytes int) []byte {
	r := rand.New(rand.NewSource(20261119))
	var sb strings.Builder
	sb.WriteString(`{"model":"bench","request":{"systemInstruction":{"role":"user","parts":[{"text":`)
	sb.WriteString(fmt.Sprintf("%q", strings.Repeat("Follow the repository guidelines and keep changes small. ", 500)))
	sb.WriteString(`}]},"contents":[`)
	sb.WriteString(`{"role":"user","parts":[{"text":"Refactor the request builder and keep the tests green."}]}`)

	image := make([]byte, screenshotBytes*3/4)
	r.Read(image)
	shot := base64.StdEncoding.EncodeToString(image)
	const turns = 30
	for turn := 0; turn < turns; turn++ {
		id := fmt.Sprintf("toolu_%02d", turn)
		var args string
		switch turn % 3 {
		case 0:
			args = fmt.Sprintf(`{"pattern":"func build%d","path":"internal","output_mode":"content","type":"go"}`, turn)
		case 1:
			args = fmt.Sprintf(`{"command":"go test ./internal/... -run Test%d","description":"Run the tests","timeout":120000}`, turn)
		default:
			args = fmt.Sprintf(`{"file_path":"/src/file_%d.go","offset":1,"limit":400}`, turn)
		}
		sb.WriteString(`,{"role":"model","parts":[{"functionCall":{"id":"` + id + `","name":"tool_` + fmt.Sprint(turn%tools) + `","args":` + args + `}}]}`)
		sb.WriteString(`,{"role":"user","parts":[{"functionResponse":{"id":"` + id + `","name":"tool_` + fmt.Sprint(turn%tools) + `","response":{"output":`)
		sb.WriteString(fmt.Sprintf("%q", strings.Repeat(fmt.Sprintf("line %d of the tool output\n", turn), 120)))
		sb.WriteString(`}}}`)
		if screenshots > 0 && turn%(turns/screenshots) == 0 {
			sb.WriteString(`,{"inlineData":{"mimeType":"image/png","data":"` + shot + `"}}`)
		}
		sb.WriteString(`]}`)
	}
	sb.WriteString(`],"tools":[{"functionDeclarations":[`)
	for i := 0; i < tools; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"name":"tool_` + fmt.Sprint(i) + `","description":`)
		sb.WriteString(fmt.Sprintf("%q", strings.Repeat(fmt.Sprintf("Tool %d does one thing well. ", i), 20)))
		sb.WriteString(`,"parametersJsonSchema":` + antigravityBenchSchema(i) + `}`)
	}
	sb.WriteString(`]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"generationConfig":{"maxOutputTokens":32000,"temperature":1,"thinkingConfig":{"includeThoughts":true,"thinkingBudget":4096}}}}`)
	return []byte(sb.String())
}

// antigravityBenchSchema is a raw tool schema of the kind MCP servers and the
// OpenAI-format clients send: draft-07 metadata, closed objects, enums, formats,
// defaults, nullable unions and nested arrays of objects.
func antigravityBenchSchema(i int) string {
	return fmt.Sprintf(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{`+
		`"path":{"type":"string","description":"Absolute path %d","minLength":1},`+
		`"mode":{"type":"string","enum":["read","write","append"],"default":"read"},`+
		`"limit":{"type":["integer","null"],"minimum":1,"maximum":1000,"default":100},`+
		`"url":{"type":"string","format":"uri"},`+
		`"flags":{"type":"array","items":{"type":"string","enum":["a","b","c"]},"maxItems":3},`+
		`"options":{"type":"object","properties":{"recursive":{"type":"boolean","default":false},"depth":{"anyOf":[{"type":"integer"},{"type":"null"}]}},"additionalProperties":false},`+
		`"entries":{"type":"array","items":{"type":"object","properties":{"key":{"type":"string","pattern":"^[a-z]+$"},"value":{"type":"string"}},"required":["key"],"additionalProperties":false}}`+
		`},"required":["path"],"additionalProperties":false}`, i)
}

package executor

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// incBenchBody is a Codex Desktop-sized turn: a long developer prompt, a tool
// list, and a hundred turns of history, ten of which carry a screenshot. It is
// about 5 MB, most of it base64 the way the real traffic is (each screenshot
// is resent every turn).
func incBenchBody(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5","instructions":"`)
	b.WriteString(strings.Repeat("Follow the repository conventions. ", 600))
	b.WriteString(`","tools":[`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"function","name":"tool_%d","description":"%s","strict":false,"parameters":{"type":"object","properties":{"cmd":{"type":"string"},"workdir":{"type":"string"}},"required":["cmd"]}}`, i, strings.Repeat("Runs a command. ", 40))
	}
	b.WriteString(`],"input":[`)
	b.WriteString(`{"type":"message","role":"developer","content":[{"type":"input_text","text":"`)
	b.WriteString(strings.Repeat("Use the tools carefully. ", 400))
	b.WriteString(`"}]}`)
	shot := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG screenshot bytes ", 20000)))
	for i := 0; i < turns; i++ {
		fmt.Fprintf(&b, `,{"type":"message","role":"user","content":[{"type":"input_text","text":"step %d: %s"}`, i, strings.Repeat("please continue ", 10))
		if i%10 == 0 {
			b.WriteString(`,{"type":"input_image","image_url":"data:image/png;base64,`)
			b.WriteString(shot)
			b.WriteString(`"}`)
		}
		b.WriteString(`]}`)
		fmt.Fprintf(&b, `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}`, strings.Repeat("Looking at the output. ", 20))
		fmt.Fprintf(&b, `,{"type":"function_call","call_id":"call_%d","name":"tool_1","arguments":"{\"cmd\":\"rg -n foo src && ls\",\"workdir\":\"/repo\"}"}`, i)
		fmt.Fprintf(&b, `,{"type":"function_call_output","call_id":"call_%d","output":"%s"}`, i, strings.Repeat("src/a.go:12: foo\\n", 100))
	}
	b.WriteString(`],"store":false,"stream":true,"prompt_cache_key":"pck-bench","client_metadata":{"turn_id":"t1"}}`)
	return []byte(b.String())
}

// BenchmarkParseCodexIncrementalRequest is what every Codex turn pays when the
// feature is on: reading the input items and the request properties.
func BenchmarkParseCodexIncrementalRequest(b *testing.B) {
	body := incBenchBody(100)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, reason := parseCodexIncrementalRequest(body)
		if reason != "" || len(req.items) == 0 {
			b.Fatalf("reason = %q", reason)
		}
	}
}

// BenchmarkCodexIncrementalTurnRetained reports what a parsed turn keeps alive
// on top of the body itself until its content is dropped, which is how long a
// turn in "on" mode holds it (until the increment is built).
func BenchmarkCodexIncrementalTurnRetained(b *testing.B) {
	body := incBenchBody(100)
	b.ReportAllocs()
	var retained uint64
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		req, _ := parseCodexIncrementalRequest(body)
		runtime.GC()
		runtime.ReadMemStats(&after)
		if after.HeapAlloc > before.HeapAlloc {
			retained += after.HeapAlloc - before.HeapAlloc
		}
		req.dropContent()
		runtime.KeepAlive(req)
	}
	b.ReportMetric(float64(retained)/float64(b.N), "retained-B/op")
	b.ReportMetric(float64(len(body)), "body-B")
}

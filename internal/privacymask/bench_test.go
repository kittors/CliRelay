package privacymask

import (
	"fmt"
	"strings"
	"testing"
)

// benchHistory is an agent's conversation of about size bytes: code, logs,
// JSON tool results and prose in English and Chinese, a few secrets and some
// personal data among them, in texts of a few KB each as a client sends them.
func benchHistory(size int) []string {
	pieces := []string{
		"func handler(w http.ResponseWriter, r *http.Request) {\n\tctx := r.Context()\n\tuser, err := store.Load(ctx, r.URL.Query().Get(\"id\"))\n\tif err != nil {\n\t\thttp.Error(w, err.Error(), 500)\n\t\treturn\n\t}\n\tjson.NewEncoder(w).Encode(user)\n}\n",
		"2026-11-05T13:37:02.118Z INFO request_id=7f3a2b1c-9d8e-4f00-a1b2-c3d4e5f60718 path=/v1/messages status=200 latency_ms=812 bytes=10422\n",
		`{"type":"tool_result","tool_use_id":"toolu_01","content":"total 48\ndrwxr-xr-x  12 dev  staff   384 Nov  5 13:30 .\n-rw-r--r--   1 dev  staff  1088 Nov  5 13:29 go.mod\n-rw-r--r--   1 dev  staff 22018 Nov  5 13:29 go.sum"}` + "\n",
		"请帮我看一下这个函数为什么在并发下会出错，日志里有很多 context deadline exceeded，max_tokens 设置为 4096，sort_key 是 created_at。\n",
		"The deploy failed because the readiness probe timed out after 90 seconds; the old slot kept serving and nothing was cut.\n",
		"OPENAI_API_KEY=" + testOpenAIKey + "\nDATABASE_URL=postgres://app:Tr0ub4dor3@db.internal:5432/app\n",
		"call me at 13800138000 or mail li.wei@acme-corp.cn about order 13912345678\n",
	}
	var texts []string
	var b strings.Builder
	total := 0
	for i := 0; total < size; i++ {
		p := pieces[i%len(pieces)]
		b.WriteString(p)
		total += len(p)
		if b.Len() >= 4096 {
			texts = append(texts, b.String()+fmt.Sprintf(" #%d", i))
			b.Reset()
		}
	}
	return append(texts, b.String())
}

func resetRuleMemo() {
	ruleMemo.Lock()
	ruleMemo.cur, ruleMemo.old = nil, nil
	ruleMemo.Unlock()
}

func BenchmarkPlanOneMegabyteHistory(b *testing.B) {
	texts := benchHistory(1 << 20)
	for _, o := range []struct {
		name string
		opts Options
	}{
		{"secrets", Options{Secrets: true}},
		{"secrets+personal", Options{Secrets: true, Personal: true}},
	} {
		// every text read for the first time: a new conversation, or the first
		// request after a restart
		b.Run(o.name+"/first", func(b *testing.B) {
			b.SetBytes(1 << 20)
			for range b.N {
				resetRuleMemo()
				NewMasker(testKey, o.opts, NewScopes().Get("bench"), "m").Plan(texts, nil)
			}
		})
		// the same history sent again, as an agent does on every turn
		b.Run(o.name+"/next-turn", func(b *testing.B) {
			scope := NewScopes().Get("bench")
			resetRuleMemo()
			NewMasker(testKey, o.opts, scope, "m").Plan(texts, nil)
			b.SetBytes(1 << 20)
			b.ResetTimer()
			for range b.N {
				NewMasker(testKey, o.opts, scope, "m").Plan(texts, nil)
			}
		})
	}
}

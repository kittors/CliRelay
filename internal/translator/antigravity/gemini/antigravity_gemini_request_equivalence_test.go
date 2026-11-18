package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	responses "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/responses"
	log "github.com/sirupsen/logrus"
)

// geminiRequestGen builds random Gemini requests covering the shapes the
// patched edits must reproduce byte for byte: every role the normalizer
// rewrites, functionCall parts with missing, short and long signatures,
// thinking parts, duplicate keys, non-object contents and parts, parts that are
// not an array, snake_case declarations with parameters, and indented JSON.
type geminiRequestGen struct{ r *rand.Rand }

func (g geminiRequestGen) str(n int) string {
	const alphabet = "abcdefghij KLMNOP 0123 \"quote\" back\\slash\nnew line 中文 🙂 <&>"
	runes := []rune(alphabet)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(runes[g.r.Intn(len(runes))])
	}
	out, _ := json.Marshal(b.String())
	return string(out)
}

func (g geminiRequestGen) sig() string {
	switch g.r.Intn(4) {
	case 0:
		return `""`
	case 1:
		return `"short-sig"`
	case 2:
		return `"skip_thought_signature_validator"`
	default:
		return `"` + strings.Repeat("S", 50+g.r.Intn(40)) + `"`
	}
}

func (g geminiRequestGen) part() string {
	switch g.r.Intn(14) {
	case 0, 1:
		return `{"text":` + g.str(20) + `}`
	case 2:
		return `{"inline_data":{"mime_type":"image/png","data":"` + strings.Repeat("QUJD", 50+g.r.Intn(200)) + `"}}`
	case 3:
		return `{"functionCall":{"name":"shell","args":{"cmd":` + g.str(10) + `}}}`
	case 4:
		return `{"functionCall":{"name":"read","args":{},"id":"c1"},"thoughtSignature":` + g.sig() + `}`
	case 5:
		return `{"thoughtSignature":` + g.sig() + `,"functionCall":{"name":"read","args":{"p":1}}}`
	case 6:
		return `{"functionResponse":{"name":"shell","response":{"result":` + g.str(30) + `}}}`
	case 7:
		return `{"text":` + g.str(15) + `,"thought":true,"thoughtSignature":` + g.sig() + `}`
	case 8:
		return `{"thought":true,"text":` + g.str(15) + `}`
	case 9:
		return `{"thought":"true","text":"x"}`
	case 10:
		return `{"functionCall":{"name":"both","args":{}},"thought":true}`
	case 11:
		return `{"thoughtSignature":"a","thoughtSignature":"b","functionCall":{"name":"dup"}}`
	case 12:
		return []string{`"loose string"`, `42`, `null`, `[1,2]`}[g.r.Intn(4)]
	default:
		return `{"functionResponse":{"name":"img","response":{"result":"ok"}},"extra":{"k":[1,{"a":"}"}]}}`
	}
}

func (g geminiRequestGen) content() string {
	if g.r.Intn(60) == 0 {
		return []string{`"loose"`, `[]`, `null`, `7`}[g.r.Intn(4)]
	}
	var parts string
	switch g.r.Intn(30) {
	case 0:
		parts = `{"0":` + g.part() + `}`
	case 1:
		parts = ""
	default:
		var list []string
		for n := g.r.Intn(5); n > 0; n-- {
			list = append(list, g.part())
		}
		parts = "[" + strings.Join(list, ",") + "]"
	}
	roles := []string{`"user"`, `"model"`, `"function"`, `""`, `"assistant"`, `"system"`, ""}
	role := roles[g.r.Intn(len(roles))]
	var fields []string
	if role != "" {
		fields = append(fields, `"role":`+role)
	}
	if parts != "" {
		fields = append(fields, `"parts":`+parts)
	}
	if g.r.Intn(8) == 0 {
		fields = append(fields, `"metadata":{"n":1}`)
	}
	g.r.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
	return "{" + strings.Join(fields, ",") + "}"
}

func (g geminiRequestGen) tools() string {
	switch g.r.Intn(5) {
	case 0:
		return ""
	case 1:
		return `,"tools":[{"function_declarations":[{"name":"a","parameters":{"type":"object","properties":{"x":{"type":"string"}}}},{"name":"b"},{"name":"c","parameters":{},"parametersJsonSchema":{"old":true}}]},{"googleSearch":{}}]`
	case 2:
		return `,"tools":[{"functionDeclarations":[{"name":"a","parametersJsonSchema":{"type":"object"}}]}]`
	case 3:
		return `,"tools":{"function_declarations":[{"name":"a","parameters":{}}]}`
	default:
		return `,"tools":[{"function_declarations":[{"parameters":{"type":"object"},"name":"z"}]},"loose"]`
	}
}

func (g geminiRequestGen) request() []byte {
	var contents []string
	for n := g.r.Intn(12); n > 0; n-- {
		contents = append(contents, g.content())
	}
	body := `{"model":"m"`
	switch g.r.Intn(25) {
	case 0: // no contents: the translator returns an empty body
	case 1:
		body += `,"contents":{"0":` + g.content() + `}`
	default:
		body += `,"contents":[` + strings.Join(contents, ",") + `]`
	}
	if g.r.Intn(3) == 0 {
		body += `,"system_instruction":{"parts":[{"text":` + g.str(10) + `}]}`
	}
	body += g.tools()
	if g.r.Intn(3) == 0 {
		body += `,"generationConfig":{"temperature":0.5}`
	}
	body += "}"
	if g.r.Intn(4) == 0 {
		var indented bytes.Buffer
		if err := json.Indent(&indented, []byte(body), "", "  "); err == nil {
			return indented.Bytes()
		}
	}
	return []byte(body)
}

func quietTranslatorLogs(t testing.TB) {
	level := log.GetLevel()
	log.SetLevel(log.ErrorLevel)
	t.Cleanup(func() { log.SetLevel(level) })
}

func TestConvertGeminiRequestToAntigravity_MatchesReference(t *testing.T) {
	quietTranslatorLogs(t)
	g := geminiRequestGen{r: rand.New(rand.NewSource(20261116))}
	for i := 0; i < 3000; i++ {
		req := g.request()
		for _, model := range []string{"gemini-3-pro-preview", "claude-sonnet-4-6"} {
			got := ConvertGeminiRequestToAntigravity(model, req, true)
			want := convertGeminiRequestToAntigravityReference(model, req, true)
			if !bytes.Equal(got, want) {
				t.Fatalf("request %d (%s) differs from the reference\nrequest: %s\n got: %s\nwant: %s", i, model, req, got, want)
			}
		}
	}
}

// codexResponsesRequest builds a Codex-style Responses request with turns tool
// calls, each followed by its output and a reasoning item, and a screenshot
// every fifth turn: the shape that pinned hk-155's CPU on 2026-11-16.
func codexResponsesRequest(turns int) []byte {
	var items []string
	items = append(items, `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}`)
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%d", i)
		items = append(items,
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking about step `+id+`"}],"encrypted_content":"`+strings.Repeat("E", 400)+`"}`,
			`{"type":"function_call","name":"shell","call_id":"`+id+`","arguments":"{\"cmd\":[\"ls\",\"-la\"]}"}`,
			`{"type":"function_call_output","call_id":"`+id+`","output":"`+strings.Repeat("line of tool output ", 100)+`"}`,
		)
		if i%5 == 4 {
			items = append(items, `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`+strings.Repeat("iVBORw0K", 2000)+`"}]}`)
		}
	}
	return []byte(`{"model":"gemini-3-pro-preview","instructions":"be brief","input":[` + strings.Join(items, ",") + `],"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"array"}}}}],"stream":true}`)
}

func codexGeminiRequest(turns int) []byte {
	return responses.ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", codexResponsesRequest(turns), true)
}

func TestConvertGeminiRequestToAntigravity_MatchesReferenceOnCodexSessions(t *testing.T) {
	for _, turns := range []int{0, 1, 7, 40} {
		req := codexGeminiRequest(turns)
		for _, model := range []string{"gemini-3-pro-preview", "claude-opus-4-6-thinking"} {
			got := ConvertGeminiRequestToAntigravity(model, req, true)
			want := convertGeminiRequestToAntigravityReference(model, req, true)
			if !bytes.Equal(got, want) {
				t.Fatalf("%d turns (%s) differs from the reference", turns, model)
			}
		}
	}
}

func allocatedBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The per-element whole-request edits made the cost grow with the square of
// the session length. Four times the history must now cost about four times as
// much, not sixteen.
func TestConvertGeminiRequestToAntigravity_CostGrowsLinearlyWithHistory(t *testing.T) {
	small, large := codexGeminiRequest(50), codexGeminiRequest(200)
	convert := func(req []byte) func() {
		return func() { ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", req, true) }
	}
	ratio := float64(allocatedBytes(convert(large))) / float64(allocatedBytes(convert(small)))
	if ratio > 8 {
		t.Fatalf("4x the history allocated %.1fx as much; the translator is quadratic again", ratio)
	}
}

func BenchmarkConvertGeminiRequestToAntigravity_CodexSession(b *testing.B) {
	req := codexGeminiRequest(300)
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", req, true)
	}
}

func BenchmarkConvertGeminiRequestToAntigravity_CodexSessionReference(b *testing.B) {
	req := codexGeminiRequest(300)
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		convertGeminiRequestToAntigravityReference("gemini-3-pro-preview", req, true)
	}
}

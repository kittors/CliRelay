package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

// responsesRequestGen builds random Responses requests covering every input
// item the translator handles: messages of each role with array or string
// content, text and image parts (data URLs with and without base64 or a mime
// type, plain URLs, missing URLs), system messages merged into
// system_instruction, batches of function calls with shuffled, duplicate or
// missing call ids and outputs of every shape, reasoning items, unknown item
// types, plus tools, generation settings, structured output and indented JSON.
type responsesRequestGen struct{ r *rand.Rand }

func (g responsesRequestGen) str(n int) string {
	const alphabet = "abcdefghij KLMNOP 0123 \"quote\" back\\slash\nnew line 中文 🙂 <&>"
	runes := []rune(alphabet)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteRune(runes[g.r.Intn(len(runes))])
	}
	out, _ := json.Marshal(b.String())
	return string(out)
}

func (g responsesRequestGen) pick(options ...string) string { return options[g.r.Intn(len(options))] }

func (g responsesRequestGen) callID() string {
	if g.r.Intn(12) == 0 {
		return ""
	}
	return fmt.Sprintf(`,"call_id":"call_%d"`, g.r.Intn(6))
}

func (g responsesRequestGen) contentItem() string {
	return g.pick(
		`{"type":"input_text","text":`+g.str(12)+`}`,
		`{"type":"output_text","text":`+g.str(12)+`}`,
		`{"text":`+g.str(8)+`}`,
		`{"type":"input_text"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,`+strings.Repeat("QUJD", 1+g.r.Intn(30))+`"}`,
		`{"type":"input_image","image_url":"data:;base64,QUJD"}`,
		`{"type":"input_image","url":"data:image/jpeg,AAAA"}`,
		`{"type":"input_image","image_url":"https://example.com/x.png"}`,
		`{"type":"input_image"}`,
		`{"type":"refusal","refusal":"no"}`,
	)
}

func (g responsesRequestGen) content() string {
	switch g.r.Intn(8) {
	case 0:
		return `,"content":` + g.str(10)
	case 1:
		return ""
	case 2:
		return `,"content":{"text":"object content"}`
	default:
		var items []string
		for n := g.r.Intn(4); n > 0; n-- {
			items = append(items, g.contentItem())
		}
		return `,"content":[` + strings.Join(items, ",") + `]`
	}
}

func (g responsesRequestGen) message() string {
	role := g.pick(`"user"`, `"assistant"`, `"system"`, `"developer"`, `"model"`, `"Tool"`, "")
	typ := g.pick(`"type":"message",`, "")
	if role == "" {
		return `{` + typ + `"id":"m"` + g.content() + `}`
	}
	return `{` + typ + `"role":` + role + g.content() + `}`
}

func (g responsesRequestGen) functionCall() string {
	args := g.pick(`"{\"cmd\":[\"ls\"]}"`, `""`, `"[1,2]"`, `"{\"nested\":{\"a\":\"}\"}}"`, `"{\"a\":1} trailing"`)
	if g.r.Intn(40) == 0 {
		args = `"{oops"` // not valid JSON; see MatchesReference
	}
	return `{"type":"function_call","name":` + g.pick(`"shell"`, `"read"`, `""`) + g.callID() + `,"arguments":` + args + `}`
}

func (g responsesRequestGen) functionCallOutput() string {
	output := g.pick(
		`,"output":`+g.str(20),
		`,"output":"{\"ok\":true}"`,
		`,"output":"42"`,
		`,"output":{"result":"structured"}`,
		`,"output":[{"type":"input_text","text":"see image"},{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]`,
		`,"output":[{"type":"input_text","text":"plain list"}]`,
		"",
	)
	return `{"type":"function_call_output"` + g.callID() + output + `}`
}

func (g responsesRequestGen) item() []string {
	switch g.r.Intn(9) {
	case 0, 1, 2:
		return []string{g.message()}
	case 3:
		var calls, outputs []string
		for n := 1 + g.r.Intn(3); n > 0; n-- {
			calls = append(calls, g.functionCall())
			outputs = append(outputs, g.functionCallOutput())
		}
		g.r.Shuffle(len(outputs), func(i, j int) { outputs[i], outputs[j] = outputs[j], outputs[i] })
		return append(calls, outputs[:g.r.Intn(len(outputs)+1)]...)
	case 4:
		return []string{g.functionCall()}
	case 5:
		return []string{g.functionCallOutput()}
	case 6:
		return []string{g.pick(
			`{"type":"reasoning","summary":[{"type":"summary_text","text":`+g.str(10)+`}],"encrypted_content":"`+strings.Repeat("E", g.r.Intn(80))+`"}`,
			`{"type":"reasoning"}`,
		)}
	case 7:
		return []string{`{"type":"web_search_call","id":"ws"}`}
	default:
		return []string{`{"role":"system","content":` + g.pick(`[{"type":"input_text","text":"sys a"},{"text":"sys b"}]`, `"sys string"`, `[]`) + `}`}
	}
}

func (g responsesRequestGen) request() []byte {
	body := `{"model":"gemini-3-pro-preview"`
	if g.r.Intn(3) == 0 {
		body += `,"instructions":` + g.str(10)
	}
	switch g.r.Intn(12) {
	case 0:
		body += `,"input":` + g.str(10)
	case 1:
	case 2:
		body += `,"input":{"type":"message"}`
	default:
		var items []string
		for n := g.r.Intn(10); n > 0; n-- {
			items = append(items, g.item()...)
		}
		body += `,"input":[` + strings.Join(items, ",") + `]`
	}
	if g.r.Intn(3) == 0 {
		body += `,"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"array"},"flag":{"type":["boolean","null"]},"n":{}}}},{"type":"web_search"},{"type":"function","name":"bare"}]`
	}
	if g.r.Intn(4) == 0 {
		body += `,"max_output_tokens":1024,"temperature":0.2,"top_p":0.9`
	}
	if g.r.Intn(5) == 0 {
		body += `,"stop":` + g.pick(`"END"`, `["A","B"]`)
	}
	if g.r.Intn(4) == 0 {
		body += `,"reasoning":{"effort":` + g.pick(`"high"`, `"none"`, `"minimal"`, `"auto"`) + `}`
	}
	if g.r.Intn(6) == 0 {
		body += `,"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`
	}
	body += `,"stream":true}`
	if g.r.Intn(4) == 0 {
		var indented bytes.Buffer
		if err := json.Indent(&indented, []byte(body), "", "  "); err == nil {
			return indented.Bytes()
		}
	}
	return []byte(body)
}

func TestConvertOpenAIResponsesRequestToGemini_MatchesReference(t *testing.T) {
	g := responsesRequestGen{r: rand.New(rand.NewSource(20261116))}
	const total = 3000
	broken := 0
	for i := 0; i < total; i++ {
		req := g.request()
		got := ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", req, true)
		want := convertOpenAIResponsesRequestToGeminiReference("gemini-3-pro-preview", req, true)
		if bytes.Equal(got, want) {
			continue
		}
		// Function call arguments that are not valid JSON are copied into args
		// as they are, so both outputs are invalid JSON. The reference then also
		// misplaces every later content, because it re-parsed the broken
		// document to append each one (see BrokenArguments below). Only valid
		// output has to match byte for byte.
		if !json.Valid(want) && !json.Valid(got) {
			broken++
			continue
		}
		t.Fatalf("request %d differs from the reference\nrequest: %s\n got: %s\nwant: %s", i, req, got, want)
	}
	if broken > total/10 {
		t.Fatalf("%d of %d requests had broken arguments; the generator no longer exercises valid input", broken, total)
	}
}

// Broken arguments used to push every later content out of the contents array:
// appending re-parsed the whole request, which no longer parsed. The request is
// still invalid JSON (the arguments are copied as they are), but the contents
// now stay in the array.
func TestConvertOpenAIResponsesRequestToGemini_BrokenArgumentsKeepLaterContentsInPlace(t *testing.T) {
	req := []byte(`{"instructions":"sys","input":[{"type":"function_call","name":"read","call_id":"c1","arguments":"{oops"},{"type":"function_call_output","call_id":"c1","output":"done"}]}`)
	got := string(ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", req, true))
	response := strings.Index(got, `"role":"function"`)
	system := strings.Index(got, `"system_instruction"`)
	if response < 0 || system < 0 || response > system {
		t.Fatalf("the tool output should stay in contents, ahead of system_instruction: %s", got)
	}
}

// codexSessionRequest builds a Codex-style Responses request with turns tool
// calls, each with its output and a reasoning item, and a screenshot every
// fifth turn: the shape that pinned hk-155's CPU on 2026-11-16.
func codexSessionRequest(turns int) []byte {
	var items []string
	items = append(items, `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}`)
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%d", i)
		items = append(items,
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"step `+id+`"}],"encrypted_content":"`+strings.Repeat("E", 400)+`"}`,
			`{"type":"function_call","name":"shell","call_id":"`+id+`","arguments":"{\"cmd\":[\"ls\",\"-la\"]}"}`,
			`{"type":"function_call_output","call_id":"`+id+`","output":"`+strings.Repeat("line of tool output ", 100)+`"}`,
		)
		if i%5 == 4 {
			items = append(items, `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`+strings.Repeat("iVBORw0K", 2000)+`"}]}`)
		}
	}
	return []byte(`{"model":"gemini-3-pro-preview","instructions":"be brief","input":[` + strings.Join(items, ",") + `],"stream":true}`)
}

func TestConvertOpenAIResponsesRequestToGemini_MatchesReferenceOnCodexSessions(t *testing.T) {
	for _, turns := range []int{0, 1, 7, 40} {
		req := codexSessionRequest(turns)
		got := ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", req, true)
		want := convertOpenAIResponsesRequestToGeminiReference("gemini-3-pro-preview", req, true)
		if !bytes.Equal(got, want) {
			t.Fatalf("%d turns differs from the reference", turns)
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

// Appending every content to the whole request, and rescanning the whole input
// for every tool output, made the cost grow with the square of the session
// length. Four times the history must now cost about four times as much.
func TestConvertOpenAIResponsesRequestToGemini_CostGrowsLinearlyWithHistory(t *testing.T) {
	small, large := codexSessionRequest(50), codexSessionRequest(200)
	convert := func(req []byte) func() {
		return func() { ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", req, true) }
	}
	ratio := float64(allocatedBytes(convert(large))) / float64(allocatedBytes(convert(small)))
	if ratio > 8 {
		t.Fatalf("4x the history allocated %.1fx as much; the translator is quadratic again", ratio)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToGemini_CodexSession(b *testing.B) {
	req := codexSessionRequest(300)
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ConvertOpenAIResponsesRequestToGemini("gemini-3-pro-preview", req, true)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToGemini_CodexSessionReference(b *testing.B) {
	req := codexSessionRequest(300)
	b.SetBytes(int64(len(req)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		convertOpenAIResponsesRequestToGeminiReference("gemini-3-pro-preview", req, true)
	}
}

package contentmoderation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

// The rewrite splices text into the bytes of a request, escapes and nested JSON
// included. These tests throw many differently shaped requests at it and check
// what must hold for every one of them, whatever the shape:
//
//   - a request with a hit that can be rewritten is rewritten, and what comes
//     out is JSON that is the same as the request everywhere except the string
//     literals that held a hit;
//   - one with a hit that cannot is refused, not passed on half done;
//   - a hit in a signed block is recorded and the block is left alone;
//   - the policy finds nothing left to replace in what it wrote.

var (
	fuzzKeys = []string{
		"messages", "content", "text", "type", "role", "input", "output", "system", "parts", "tool_calls", "function", "arguments",
		"name", "id", "call_id", "summary", "instructions", "prompt", "contents", "systemInstruction", "request", "source",
		"data", "title", "refusal", "thinking", "signature", "encrypted_content", "tool_use_id", "status", "result", "error",
		"functionCall", "functionResponse", "args", "response", "code", "reasoning_content", "thought", "extra", "BADWORD", "badword", "k1", "k2",
	}
	fuzzTypes = []string{
		"text", "input_text", "output_text", "message", "summary_text", "thinking", "tool_use", "server_tool_use", "tool_result",
		"document", "image", "function_call", "function_call_output", "reasoning", "mcp_call", "unknown_x", "",
	}
	fuzzRoles   = []string{"user", "assistant", "system", "developer", "tool", "model", ""}
	fuzzStrings = []string{
		"hello world", "plain text", "BADWORD", "say BADWORD now", "BaDwOrD in mixed case", "ＢＡＤＷＯＲＤ full width", "BAD​WORD zero width",
		"中文内容 BADWORD 后面", "emoji 😀 BADWORD 😀", "line1\nline2 BADWORD\tend", "quote \" and backslash \\ BADWORD",
		"<system-reminder>reminder BADWORD</system-reminder>", "", " ", "   spaced   out   ", "data:image/png;base64,AAAA",
		"{\"nested\":\"BADWORD\"}", "[\"BADWORD\"]", "x", "é́ combining", "a\u0000b BADWORD", "tab\there", "ǅ ligature ﬁ BADWORD",
		"繁體中文違禁BADWORD", "<b>BADWORD</b> & more", "BADWORDBADWORD", "ends with BADWORD", "BADWORD",
	}
)

type fuzzGen struct {
	r    *rand.Rand
	uniq int
}

func (g *fuzzGen) str() string {
	g.uniq++
	base := fuzzStrings[g.r.Intn(len(fuzzStrings))]
	if base == "" || base == " " || strings.HasPrefix(base, "data:") {
		return base
	}
	return fmt.Sprintf("%s #%d", base, g.uniq)
}

func (g *fuzzGen) value(depth int) any {
	if depth <= 0 {
		return g.str()
	}
	switch g.r.Intn(12) {
	case 0, 1, 2, 3:
		return g.str()
	case 4:
		return float64(g.r.Intn(100))
	case 5:
		return g.r.Intn(2) == 0
	case 6:
		return nil
	case 7, 8:
		arr := make([]any, 0, 4)
		for i := g.r.Intn(4); i > 0; i-- {
			arr = append(arr, g.value(depth-1))
		}
		return arr
	default:
		return g.object(depth - 1)
	}
}

func (g *fuzzGen) object(depth int) map[string]any {
	obj := make(map[string]any)
	for i := g.r.Intn(5); i > 0; i-- {
		key := fuzzKeys[g.r.Intn(len(fuzzKeys))]
		switch key {
		case "type":
			obj[key] = fuzzTypes[g.r.Intn(len(fuzzTypes))]
		case "role":
			obj[key] = fuzzRoles[g.r.Intn(len(fuzzRoles))]
		case "thought":
			obj[key] = g.r.Intn(2) == 0
		case "arguments":
			// Half of the arguments in the wild are a string that holds JSON.
			if g.r.Intn(2) == 0 {
				b, _ := json.Marshal(g.object(3))
				obj[key] = string(b)
			} else {
				obj[key] = g.value(depth)
			}
		default:
			obj[key] = g.value(depth)
		}
	}
	return obj
}

func (g *fuzzGen) message() map[string]any {
	m := map[string]any{"role": fuzzRoles[g.r.Intn(len(fuzzRoles))]}
	switch g.r.Intn(4) {
	case 0:
		m["content"] = g.str()
	case 1:
		blocks := []any{}
		for i := g.r.Intn(3) + 1; i > 0; i-- {
			b := g.object(2)
			if g.r.Intn(2) == 0 {
				b["type"] = fuzzTypes[g.r.Intn(len(fuzzTypes))]
			}
			if g.r.Intn(2) == 0 {
				b["text"] = g.str()
			}
			if g.r.Intn(6) == 0 {
				// A block the model signed.
				b = map[string]any{"type": "thinking", "thinking": g.str(), "signature": "SIG=="}
			}
			blocks = append(blocks, b)
		}
		m["content"] = blocks
	case 2:
		m["content"] = g.object(2)
	default:
		m["content"] = nil
	}
	if g.r.Intn(3) == 0 {
		args, _ := json.Marshal(g.object(3))
		m["tool_calls"] = []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": string(args)}}}
	}
	return m
}

func (g *fuzzGen) top() map[string]any {
	doc := map[string]any{"model": "m"}
	msgs := []any{}
	for i := g.r.Intn(4); i >= 0; i-- {
		msgs = append(msgs, g.message())
	}
	switch g.r.Intn(5) {
	case 0:
		doc["messages"] = msgs
	case 1:
		doc["system"] = g.value(2)
		doc["messages"] = msgs
	case 2:
		doc["instructions"] = g.value(2)
		doc["input"] = msgs
	case 3:
		contents := []any{}
		for i := g.r.Intn(3) + 1; i > 0; i-- {
			parts := []any{}
			for j := g.r.Intn(3) + 1; j > 0; j-- {
				part := g.object(2)
				if g.r.Intn(6) == 0 {
					part = map[string]any{"text": g.str(), "thought": true, "thoughtSignature": "SIG"}
				}
				parts = append(parts, part)
			}
			contents = append(contents, map[string]any{"role": fuzzRoles[g.r.Intn(len(fuzzRoles))], "parts": parts})
		}
		doc["contents"] = contents
		doc["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": g.str()}}}
		if g.r.Intn(3) == 0 {
			doc = map[string]any{"project": "p", "request": doc}
		}
	default:
		doc = g.object(4)
	}
	if g.r.Intn(4) == 0 {
		doc["prompt"] = g.value(1)
	}
	return doc
}

// payloads makes n documents. Some write non-ASCII text as \u escapes, which is
// how many clients write it, and a few carry a byte that is not UTF-8, which no
// rewrite can place.
func fuzzPayloads(n int, seed int64) [][]byte {
	g := &fuzzGen{r: rand.New(rand.NewSource(seed))}
	out := make([][]byte, 0, n)
	for len(out) < n {
		b, err := json.Marshal(g.top())
		if err != nil {
			continue
		}
		s := string(b)
		if g.r.Intn(4) == 0 {
			var sb strings.Builder
			for _, r := range s {
				if r > 127 && r < 0x10000 {
					fmt.Fprintf(&sb, "\\u%04x", r)
				} else {
					sb.WriteRune(r)
				}
			}
			s = sb.String()
		}
		if g.r.Intn(60) == 0 && strings.Contains(s, "BADWORD") {
			s = strings.Replace(s, "BADWORD", "BAD\xffWORD BADWORD", 1)
		}
		out = append(out, []byte(s))
	}
	return out
}

// rootRange is where in the request document a unit's rewrite lands: the
// literal itself, or, for text inside a tool call's arguments, the string
// holding those arguments.
func rootRange(unit wordUnit) (int, int) {
	doc := unit.doc
	if doc.parent == nil {
		return unit.lit0, unit.lit1
	}
	for doc.parent.parent != nil {
		doc = doc.parent
	}
	return doc.lit0, doc.lit1
}

// nodeCount counts the values of a document, so a rewrite that loses or adds a
// member shows.
func nodeCount(v gjson.Result) int {
	n := 1
	if v.IsObject() || v.IsArray() {
		v.ForEach(func(_, child gjson.Result) bool { n += nodeCount(child); return true })
	}
	return n
}

func endOfLiteral(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

func TestWordGuardRewriteProperties(t *testing.T) {
	count := 1500
	if v, err := strconv.Atoi(os.Getenv("WORDGUARD_FUZZ_N")); err == nil && v > 0 {
		count = v
	}
	formats := []sdktranslator.Format{
		sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI,
	}
	scopes := []string{WordScanLastUser, WordScanAllUser, WordScanWithSystem, WordScanWithTools}
	payloads := fuzzPayloads(count, 20261101)

	type stats struct{ cases, replaced, refused, recorded int }
	var total stats
	failures := 0
	for _, fold := range []bool{false, true} {
		for _, scope := range scopes {
			policy := policyOf(t, WordScopeTenant, "", func(p *WordPolicy) {
				p.Action, p.ScanScope, p.FoldTraditionalChinese = WordActionReplace, scope, boolPtr(fold)
			}, kw("BADWORD"))
			contexts := buildWordContexts(nil, policy, nil)
			for _, payload := range payloads {
				for _, format := range formats {
					if failures >= 8 {
						t.Fatalf("stopping after %d failures", failures)
					}
					total.cases++
					units := extractWordUnits(format, payload)
					verdict := evaluateWords(format, payload, contexts, wordEvalOptions{})

					// What the policy should find, worked out from the units on their own.
					var replaceable, signedOnly []int
					for _, ui := range unitsInScope(units, scope) {
						text := normalizeForMatch(units[ui].text, fold)
						if len(contexts[0].matcher.find(text, findOptions{}).hits) == 0 {
							continue
						}
						if units[ui].signed {
							signedOnly = append(signedOnly, ui)
						} else {
							replaceable = append(replaceable, ui)
						}
					}
					where := fmt.Sprintf("format=%s scope=%s fold=%v\n in=%s", format, scope, fold, payload)
					bad := func(msg string, args ...any) {
						failures++
						t.Errorf(msg+"\n"+where+"\n verdict=%+v", append(args, verdict)...)
					}

					if len(replaceable) == 0 {
						want := WordVerdictAllow
						if len(signedOnly) > 0 {
							want = WordVerdictLog
							total.recorded++
						}
						if verdict.Action != want || verdict.Blocked || verdict.Payload != nil {
							bad("no hit to replace, want %s", want)
						}
						continue
					}

					unrewritable := false
					for _, ui := range replaceable {
						// A text that is not UTF-8 cannot be traced back to the bytes it came from.
						if !units[ui].rewritable || !utf8.ValidString(units[ui].text) {
							unrewritable = true
						}
					}
					if unrewritable {
						total.refused++
						if !verdict.Blocked || verdict.Reason != WordReasonUnrewritable || verdict.Payload != nil {
							bad("a hit that cannot be rewritten must refuse the request")
						} else if verdict.Field == "" || !strings.Contains(verdict.BlockMessage, verdict.Field[:min(len(verdict.Field), 200)]) {
							bad("the refusal must say where the text is")
						}
						continue
					}

					total.replaced++
					if verdict.Blocked || verdict.Action != WordVerdictReplace || verdict.Payload == nil {
						bad("a hit that can be rewritten must be rewritten")
						continue
					}
					out := verdict.Payload
					if !gjson.ValidBytes(out) {
						bad("the rewrite is not valid JSON:\nout=%s", out)
						continue
					}
					if before, after := nodeCount(gjson.ParseBytes(payload)), nodeCount(gjson.ParseBytes(out)); before != after {
						bad("the rewrite changed the shape of the request: %d values before, %d after\nout=%s", before, after, out)
						continue
					}

					// Outside the literals that held a hit, the bytes are the request's own.
					type span struct{ from, to int }
					seen := make(map[int]bool)
					var spans []span
					for _, ui := range replaceable {
						from, to := rootRange(units[ui])
						if !seen[from] {
							seen[from] = true
							spans = append(spans, span{from, to})
						}
					}
					sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
					pos, outPos, same := 0, 0, true
					for _, sp := range spans {
						gap := payload[pos:sp.from]
						if outPos+len(gap) > len(out) || !bytes.Equal(out[outPos:outPos+len(gap)], gap) {
							same = false
							break
						}
						outPos += len(gap)
						end := endOfLiteral(out, outPos)
						if end < 0 {
							same = false
							break
						}
						if text, ok := decodeJSONLiteral(out[outPos:end]); !ok || !utf8.ValidString(text) {
							bad("a rewritten literal is not text: %q", out[outPos:end])
						}
						outPos, pos = end, sp.to
					}
					if !same || !bytes.Equal(out[outPos:], payload[pos:]) {
						bad("bytes outside the literals that held a hit changed\nout=%s", out)
						continue
					}

					// Nothing is left to replace in what was written.
					if again := evaluateWords(format, out, contexts, wordEvalOptions{}); again.Blocked || again.Action == WordVerdictReplace {
						bad("the rewrite still hits when read again: %+v\nout=%s", again, out)
					}
				}
			}
		}
	}
	t.Logf("%+v", total)
	if total.replaced == 0 || total.refused == 0 || total.recorded == 0 {
		t.Fatalf("the generated requests did not exercise every outcome: %+v", total)
	}
}

package translatortest

import (
	"fmt"
	"math/rand"
	"strings"
)

// ClaudeRequest returns a random Anthropic Messages request covering what the
// translators read: a system prompt as a string or blocks, user and assistant
// messages (and non-string or unknown roles) with string or block content,
// text (empty and blank too), base64 and URL images, tool_use with object,
// string or missing input, tool_result with string, block, image or missing
// content for known and unknown ids, thinking blocks with signatures of each
// family and group prefix, redacted thinking, documents, tools of each shape,
// tool_choice, thinking config, sampling and output_config, sometimes
// indented.
func ClaudeRequest(r *rand.Rand) []byte {
	g := gen{r}
	body := `{"model":"claude-sonnet-4-5","max_tokens":` + g.pick(`1024`, `32000`)
	switch r.Intn(5) {
	case 0:
		body += `,"system":` + g.text()
	case 1:
		body += `,"system":[` + strings.Join([]string{
			`{"type":"text","text":` + g.text() + g.pick(``, `,"cache_control":{"type":"ephemeral"}`) + `}`,
			g.pick(`{"type":"text","text":"second"}`, `{"type":"text","text":7}`, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`, `{"type":"other"}`),
		}[:1+r.Intn(2)], ",") + `]`
	case 2:
		body += `,"system":` + g.pick(`[]`, `7`, `{"text":"object"}`)
	}
	switch r.Intn(12) {
	case 0:
	case 1:
		body += `,"messages":{"role":"user"}`
	default:
		var msgs []string
		for n := r.Intn(12); n > 0; n-- {
			msgs = append(msgs, g.claudeMessages()...)
		}
		body += `,"messages":[` + strings.Join(msgs, ",") + `]`
	}
	if g.chance(3) {
		body += `,"tools":[` + g.claudeTools() + `]`
	}
	if g.chance(4) {
		body += `,"tool_choice":` + g.pick(`{"type":"auto"}`, `{"type":"any"}`, `{"type":"none"}`, `{"type":"tool","name":"shell"}`, `{"type":"tool"}`)
	}
	if g.chance(3) {
		body += `,"thinking":` + g.pick(`{"type":"enabled","budget_tokens":2048}`, `{"type":"enabled","budget_tokens":"lots"}`, `{"type":"enabled"}`, `{"type":"adaptive"}`, `{"type":"disabled"}`, `true`)
	}
	if g.chance(4) {
		body += `,"temperature":` + g.pick(`0.2`, `"warm"`) + `,"top_p":0.9,"top_k":40`
	}
	if g.chance(5) {
		body += `,"stop_sequences":` + g.pick(`["END"]`, `[]`, `"END"`)
	}
	if g.chance(5) {
		body += `,"metadata":{"user_id":"user_abc"}`
	}
	if g.chance(6) {
		body += `,"output_config":{"format":` + g.pick(`{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}`, `{"type":"json"}`) + `}`
	}
	body += `,"stream":true}`
	return g.indent(body)
}

func (g gen) claudeThinking() string {
	sig := g.pick(
		`,"signature":"claude#`+g.claudeSignature()+`"`,
		`,"signature":"gemini#`+strings.Repeat("G", 60)+`"`,
		`,"signature":"gemini#skip_thought_signature_validator"`,
		`,"signature":"gpt#`+strings.Repeat("P", 60)+`"`,
		`,"signature":"`+g.claudeSignature()+`"`,
		`,"signature":"short"`,
		`,"signature":""`,
		``,
	)
	return `{"type":"thinking","thinking":` + g.pick(g.text(), `"   "`, `""`) + sig + `}`
}

func (g gen) claudeImage() string {
	return g.pick(
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+g.base64Data()+`"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"","data":"QUJD"}}`,
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":""}}`,
		`{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}`,
		`{"type":"image","url":"https://example.com/y.png"}`,
		`{"type":"image"}`,
	)
}

func (g gen) claudeToolUse() string {
	input := g.pick(`,"input":{"cmd":["ls"]}`, `,"input":{}`, `,"input":"{\"path\":\"a.go\"}"`, `,"input":"not json"`, `,"input":[1,2]`, ``, `,"input":{"nested":{"a":"<b>&"}}`)
	id := `,"id":` + g.pick(`"toolu_1"`, `"toolu_2"`, `"shell-call-1"`, `"read-x-y"`, `""`)
	if g.chance(12) {
		id = ""
	}
	return `{"type":"tool_use"` + id + `,"name":` + g.pick(`"shell"`, `"read"`, `""`, `"  "`) + input + `}`
}

func (g gen) claudeToolResult() string {
	content := g.pick(
		`,"content":`+g.text(),
		`,"content":"{\"ok\":true}"`,
		`,"content":[{"type":"text","text":"done"}]`,
		`,"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]`,
		`,"content":[{"type":"text","text":"see"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]`,
		`,"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+g.base64Data()+`"}}]`,
		`,"content":{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`,
		`,"content":{"result":"structured"}`,
		`,"content":[]`,
		`,"content":42`,
		``,
	)
	isErr := g.pick(``, `,"is_error":true`)
	return `{"type":"tool_result","tool_use_id":` + g.pick(`"toolu_1"`, `"toolu_2"`, `"shell-call-1"`, `"read-x-y"`, `"unknown"`, `""`) + content + isErr + `}`
}

func (g gen) claudeBlock(assistant bool) string {
	switch g.r.Intn(10) {
	case 0, 1:
		return `{"type":"text","text":` + g.pick(g.text(), `""`, `"  "`) + g.pick(``, `,"cache_control":{"type":"ephemeral"}`) + `}`
	case 2:
		return g.claudeImage()
	case 3:
		if assistant {
			return g.claudeThinking()
		}
		return g.claudeToolResult()
	case 4:
		return g.claudeToolUse()
	case 5:
		return g.claudeToolResult()
	case 6:
		if assistant || g.chance(4) {
			return g.claudeThinking()
		}
		return `{"type":"text","text":"user text"}`
	case 7:
		return g.pick(`{"type":"redacted_thinking","data":"`+g.base64Data()+`"}`, `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"QUJD"}}`, `{"type":"search_result"}`, `{"text":"untyped"}`)
	default:
		return `{"type":"text","text":` + g.text() + `}`
	}
}

func (g gen) claudeContent(assistant bool) string {
	switch g.r.Intn(6) {
	case 0:
		return `,"content":` + g.pick(g.text(), `""`)
	case 1:
		return g.pick(``, `,"content":7`, `,"content":[]`)
	default:
		var blocks []string
		for n := 1 + g.r.Intn(4); n > 0; n-- {
			blocks = append(blocks, g.claudeBlock(assistant))
		}
		return `,"content":[` + strings.Join(blocks, ",") + `]`
	}
}

func (g gen) claudeMessages() []string {
	switch g.r.Intn(10) {
	case 0, 1, 2:
		return []string{`{"role":"user"` + g.claudeContent(false) + `}`}
	case 3, 4, 5:
		return []string{`{"role":"assistant"` + g.claudeContent(true) + `}`}
	case 6, 7:
		// A tool round as Claude Code sends it: thinking, calls, then the
		// results in one user message.
		var calls, results []string
		for n := 1 + g.r.Intn(3); n > 0; n-- {
			calls = append(calls, g.claudeToolUse())
			results = append(results, g.claudeToolResult())
		}
		head := ""
		if g.chance(2) {
			head = g.claudeThinking() + ","
		}
		return []string{
			`{"role":"assistant","content":[` + head + strings.Join(calls, ",") + `]}`,
			`{"role":"user","content":[` + strings.Join(results, ",") + `]}`,
		}
	default:
		return []string{g.pick(`{"role":"system","content":"inline system"}`, `{"role":7,"content":"x"}`, `{"content":"no role"}`, `{"role":"tool","content":"t"}`, `"scalar"`)}
	}
}

func (g gen) claudeTools() string {
	var tools []string
	for n := g.r.Intn(5); n > 0; n-- {
		tools = append(tools, g.pick(
			`{"name":"shell","description":"run","input_schema":`+g.schema()+`}`,
			`{"name":"read","input_schema":`+g.schema()+`,"strict":true,"cache_control":{"type":"ephemeral"},"input_examples":[{"cmd":["ls"]}]}`,
			`{"type":"custom","name":"custom","input_schema":{"type":"object","properties":{}}}`,
			`{"name":"bare"}`,
			`{"name":"bad","input_schema":"not a schema"}`,
			`{"type":"web_search_20250305","name":"web_search","max_uses":5}`,
			`{"name":"deferred","input_schema":{"type":"object"},"defer_loading":true}`,
		))
	}
	return strings.Join(tools, ",")
}

// ClaudeCodeSession is an Anthropic Messages request with turns tool calls as
// Claude Code sends them, each with its result, and a screenshot result every
// fifth turn.
func ClaudeCodeSession(turns int) []byte {
	msgs := []string{`{"role":"user","content":[{"type":"text","text":"fix the build"}]}`}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		result := `"` + strings.Repeat("line of tool output ", 100) + `"`
		if i%5 == 4 {
			result = `[{"type":"text","text":"screenshot"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("iVBORw0K", 2000) + `"}}]`
		}
		msgs = append(msgs,
			`{"role":"assistant","content":[{"type":"text","text":"step `+id+`"},{"type":"tool_use","id":"`+id+`","name":"shell","input":{"cmd":["ls","-la"]}}]}`,
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+id+`","content":`+result+`}]}`,
		)
	}
	return []byte(`{"model":"claude-sonnet-4-5","max_tokens":32000,"system":[{"type":"text","text":"be brief"}],"messages":[` + strings.Join(msgs, ",") + `],"tools":[{"name":"shell","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"array"}}}}],"stream":true}`)
}

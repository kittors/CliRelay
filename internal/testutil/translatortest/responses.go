package translatortest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
)

// sessionSignature is a 400-byte encrypted_content shaped like an Anthropic
// thinking signature, so Claude upstreams replay the session's reasoning too.
var sessionSignature = base64.StdEncoding.EncodeToString(append([]byte{0x12}, bytes.Repeat([]byte("E"), 299)...))

// ResponsesRequest returns a random OpenAI Responses request covering the
// input items the translators handle: messages of each role (with and without
// a type) holding array, string, object or no content, text, image and file
// parts, system messages, batches of function calls with shuffled, duplicate
// or missing call ids and outputs of every shape, reasoning items carrying
// each family's encrypted_content, unknown items and scalars, plus tools,
// tool_choice, generation settings, structured output and indented JSON.
func ResponsesRequest(r *rand.Rand) []byte {
	g := gen{r}
	body := `{"model":"gpt-5.4"`
	switch r.Intn(4) {
	case 0:
		body += `,"instructions":` + g.text()
	case 1:
		body += `,"instructions":` + g.pick(`42`, `["a"]`, `null`)
	}
	switch r.Intn(14) {
	case 0:
		body += `,"input":` + g.text()
	case 1:
	case 2:
		body += `,"input":{"type":"message"}`
	default:
		var items []string
		for n := r.Intn(12); n > 0; n-- {
			items = append(items, g.responsesItem()...)
		}
		body += `,"input":[` + strings.Join(items, ",") + `]`
	}
	if g.chance(3) {
		body += `,"tools":[` + g.responsesTools() + `]`
	}
	if g.chance(3) {
		body += `,"tool_choice":` + g.pick(`"auto"`, `"none"`, `"required"`, `"other"`, `{"type":"function","name":"shell"}`, `{"type":"function","function":{"name":"read"}}`, `{"type":"web_search"}`, `7`)
	}
	if g.chance(4) {
		body += `,"max_output_tokens":1024,"temperature":0.2,"top_p":0.9`
	}
	if g.chance(5) {
		body += `,"parallel_tool_calls":` + g.pick(`true`, `false`)
	}
	if g.chance(4) {
		body += `,"reasoning":{"effort":` + g.pick(`"high"`, `"none"`, `"minimal"`, `"low"`, `"medium"`, `"xhigh"`, `"auto"`, `""`, `" HIGH "`) + `}`
	}
	if g.chance(6) {
		body += `,"text":{"format":` + g.pick(`{"type":"json_schema","name":"out","schema":{"type":"object","properties":{"a":{"type":"string"}}}}`, `{"type":"json_object"}`, `{"type":"text"}`) + `}`
	}
	if g.chance(5) {
		body += `,"stop":` + g.pick(`"END"`, `["A","B"]`)
	}
	body += `,"stream":true}`
	return g.indent(body)
}

func (g gen) responsesContentPart() string {
	return g.pick(
		`{"type":"input_text","text":`+g.text()+`}`,
		`{"type":"output_text","text":`+g.text()+`}`,
		`{"type":"input_text","text":""}`,
		`{"text":`+g.text()+`}`,
		`{"type":"input_text"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,`+g.base64Data()+`"}`,
		`{"type":"input_image","image_url":"data:;base64,QUJD"}`,
		`{"type":"input_image","image_url":"data:image/png;base64,"}`,
		`{"type":"input_image","url":"data:image/jpeg,AAAA"}`,
		`{"type":"input_image","image_url":"https://example.com/x.png"}`,
		`{"type":"input_image"}`,
		`{"type":"input_file","file_data":"data:application/pdf;base64,`+g.base64Data()+`","filename":"a.pdf"}`,
		`{"type":"input_file","file_data":"`+g.base64Data()+`"}`,
		`{"type":"input_file"}`,
		`{"type":"refusal","refusal":"no"}`,
	)
}

func (g gen) responsesContent() string {
	switch g.r.Intn(9) {
	case 0:
		return `,"content":` + g.text()
	case 1:
		return ""
	case 2:
		return `,"content":{"text":"object content"}`
	case 3:
		return `,"content":[]`
	default:
		var parts []string
		for n := 1 + g.r.Intn(4); n > 0; n-- {
			parts = append(parts, g.responsesContentPart())
		}
		return `,"content":[` + strings.Join(parts, ",") + `]`
	}
}

func (g gen) responsesMessage() string {
	role := g.pick(`"user"`, `"user"`, `"assistant"`, `"assistant"`, `"system"`, `"developer"`, `"model"`, `"Tool"`, `"SYSTEM"`, "")
	typ := g.pick(`"type":"message",`, "")
	if role == "" {
		return `{` + typ + `"id":"m"` + g.responsesContent() + `}`
	}
	return `{` + typ + `"role":` + role + g.responsesContent() + `}`
}

func (g gen) responsesCallID() string {
	if g.chance(12) {
		return ""
	}
	return `,"call_id":` + g.id("call_")
}

func (g gen) responsesFunctionCall() string {
	return `{"type":"function_call","name":` + g.pick(`"shell"`, `"read"`, `""`) + g.responsesCallID() + `,"arguments":` + g.args() + `}`
}

func (g gen) responsesFunctionCallOutput() string {
	output := g.pick(
		`,"output":`+g.text(),
		`,"output":"{\"ok\":true}"`,
		`,"output":"42"`,
		`,"output":{"result":"structured"}`,
		`,"output":[{"type":"input_text","text":"see image"},{"type":"input_image","image_url":"data:image/png;base64,QUJD"}]`,
		`,"output":[{"type":"input_text","text":"plain list"}]`,
		`,"output":[]`,
		"",
	)
	return `{"type":"function_call_output"` + g.responsesCallID() + output + `}`
}

func (g gen) responsesReasoning() string {
	var fields []string
	switch g.r.Intn(4) {
	case 0:
		fields = append(fields, `"summary":[{"type":"summary_text","text":`+g.text()+`}]`)
	case 1:
		fields = append(fields, `"summary":[{"type":"summary_text","text":`+g.text()+`},{"type":"summary_text","text":7},{"type":"other","text":"x"}]`)
	case 2:
		fields = append(fields, `"summary":[],"content":[{"type":"reasoning_text","text":`+g.text()+`}]`)
	}
	if !g.chance(5) {
		fields = append(fields, `"encrypted_content":`+g.signature())
	}
	if len(fields) == 0 {
		return `{"type":"reasoning"}`
	}
	return `{"type":"reasoning","id":"rs",` + strings.Join(fields, ",") + `}`
}

func (g gen) responsesItem() []string {
	switch g.r.Intn(11) {
	case 0, 1, 2:
		return []string{g.responsesMessage()}
	case 3:
		var calls, outputs []string
		for n := 1 + g.r.Intn(3); n > 0; n-- {
			calls = append(calls, g.responsesFunctionCall())
			outputs = append(outputs, g.responsesFunctionCallOutput())
		}
		g.r.Shuffle(len(outputs), func(i, j int) { outputs[i], outputs[j] = outputs[j], outputs[i] })
		return append(calls, outputs[:g.r.Intn(len(outputs)+1)]...)
	case 4:
		return []string{g.responsesFunctionCall()}
	case 5:
		return []string{g.responsesFunctionCallOutput()}
	case 6, 7:
		// Reasoning followed by what it led to, as Codex replays a turn.
		items := []string{g.responsesReasoning()}
		for n := g.r.Intn(3); n > 0; n-- {
			if g.chance(2) {
				items = append(items, g.responsesFunctionCall())
			} else {
				items = append(items, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":`+g.text()+`}]}`)
			}
		}
		return items
	case 8:
		return []string{g.pick(`{"type":"web_search_call","id":"ws"}`, `"scalar"`, `17`, `{}`)}
	default:
		return []string{`{"role":"system","content":` + g.pick(`[{"type":"input_text","text":"sys a"},{"text":"sys b"}]`, `"sys string"`, `[]`, `""`) + `}`}
	}
}

func (g gen) responsesTools() string {
	var tools []string
	for n := g.r.Intn(4); n > 0; n-- {
		tools = append(tools, g.pick(
			`{"type":"function","name":"shell","description":"run","parameters":`+g.schema()+`}`,
			`{"type":"function","name":"read","parametersJsonSchema":`+g.schema()+`}`,
			`{"type":"function","name":"bare"}`,
			`{"name":"untyped","parameters":{"type":"object"}}`,
			`{"type":"web_search"}`,
			`{"type":"image_generation"}`,
		))
	}
	return strings.Join(tools, ",")
}

// CodexResponsesSession is a Codex-style Responses request with turns tool
// calls, each with its output and a reasoning item, and a screenshot every
// fifth turn: the shape that pinned hk-155's CPU on 2026-11-16.
func CodexResponsesSession(turns int) []byte {
	var items []string
	items = append(items, `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}`)
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%d", i)
		items = append(items,
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"step `+id+`"}],"encrypted_content":"`+sessionSignature+`"}`,
			`{"type":"function_call","name":"shell","call_id":"`+id+`","arguments":"{\"cmd\":[\"ls\",\"-la\"]}"}`,
			`{"type":"function_call_output","call_id":"`+id+`","output":"`+strings.Repeat("line of tool output ", 100)+`"}`,
		)
		if i%5 == 4 {
			items = append(items, `{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`+strings.Repeat("iVBORw0K", 2000)+`"}]}`)
		}
	}
	return []byte(`{"model":"gpt-5.4","instructions":"be brief","input":[` + strings.Join(items, ",") + `],"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"array"}}}}],"stream":true}`)
}

package translatortest

import (
	"fmt"
	"math/rand"
	"strings"
)

// ChatRequest returns a random OpenAI Chat Completions request covering what
// the translators read: system and developer messages with string, object or
// array content (alone or among others), user messages with text, image and
// file parts, assistant messages with text, inline images and tool calls of
// every shape (missing, duplicate or unmatched ids, invalid arguments), tool
// messages with text, JSON and image results, unknown roles, tools of each
// kind, reasoning effort, sampling, modalities, image config and response
// formats, sometimes indented.
func ChatRequest(r *rand.Rand) []byte {
	g := gen{r}
	body := `{"model":"gpt-4o"`
	switch r.Intn(12) {
	case 0:
	case 1:
		body += `,"messages":{"role":"user"}`
	case 2:
		// A lone system message is sent as the user turn.
		body += `,"messages":[` + g.chatSystem() + `]`
	default:
		var msgs []string
		for n := r.Intn(12); n > 0; n-- {
			msgs = append(msgs, g.chatMessages()...)
		}
		body += `,"messages":[` + strings.Join(msgs, ",") + `]`
	}
	if g.chance(3) {
		body += `,"tools":[` + g.chatTools() + `]`
	}
	if g.chance(3) {
		body += `,"tool_choice":` + g.pick(`"auto"`, `"none"`, `"required"`, `{"type":"function","function":{"name":"shell"}}`)
	}
	if g.chance(3) {
		body += `,"reasoning_effort":` + g.pick(`"none"`, `"auto"`, `"low"`, `"medium"`, `"high"`, `"xhigh"`, `""`, `"bogus"`, `3`)
	}
	if g.chance(4) {
		body += `,"temperature":` + g.pick(`0.2`, `"hot"`) + `,"top_p":0.9,"top_k":` + g.pick(`40`, `"k"`) + `,"max_tokens":` + g.pick(`1024`, `"many"`)
	}
	if g.chance(6) {
		body += `,"n":` + g.pick(`1`, `2`, `"two"`)
	}
	if g.chance(6) {
		body += `,"modalities":` + g.pick(`["text","image"]`, `["image"]`, `["audio"]`, `"text"`)
	}
	if g.chance(8) {
		body += `,"image_config":{"aspect_ratio":` + g.pick(`"16:9"`, `1`) + `,"image_size":"1K"}`
	}
	if g.chance(6) {
		body += `,"response_format":` + g.pick(`{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}`, `{"type":"json_object"}`, `{"type":"text"}`)
	}
	if g.chance(5) {
		body += `,"stop":` + g.pick(`"END"`, `["A","B"]`)
	}
	body += `,"stream":true}`
	return g.indent(body)
}

func (g gen) chatSystem() string {
	role := g.pick(`"system"`, `"developer"`)
	return `{"role":` + role + `,"content":` + g.pick(
		g.text(),
		`{"type":"text","text":`+g.text()+`}`,
		`{"type":"image_url"}`,
		`[{"type":"text","text":"sys a"},{"type":"text","text":`+g.text()+`},{"text":"untyped"}]`,
		`[]`,
		`7`,
	) + `}`
}

func (g gen) chatUserPart() string {
	return g.pick(
		`{"type":"text","text":`+g.text()+`}`,
		`{"type":"text","text":""}`,
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,`+g.base64Data()+`"}}`,
		`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}`,
		`{"type":"image_url","image_url":{"url":"data:;base64,AA"}}`,
		`{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}`,
		`{"type":"image_url","image_url":{"url":"abc"}}`,
		`{"type":"image_url"}`,
		`{"type":"file","file":{"filename":"a.pdf","file_data":"`+g.base64Data()+`"}}`,
		`{"type":"file","file":{"filename":"notes.txt","file_data":"aGVsbG8="}}`,
		`{"type":"file","file":{"filename":"blob.unknownext","file_data":"AAAA"}}`,
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}`,
	)
}

func (g gen) chatContentArray(part func() string) string {
	var parts []string
	for n := g.r.Intn(4); n > 0; n-- {
		parts = append(parts, part())
	}
	return `[` + strings.Join(parts, ",") + `]`
}

func (g gen) chatToolCall() string {
	typ := `"function"`
	if g.chance(15) {
		typ = `"custom"`
	}
	id := `,"id":` + g.id("call_")
	if g.chance(12) {
		id = ""
	}
	return `{"type":` + typ + id + `,"function":{"name":` + g.pick(`"shell"`, `"read"`, `""`) + `,"arguments":` + g.args() + `}}`
}

func (g gen) chatAssistant() string {
	msg := `{"role":"assistant"`
	switch g.r.Intn(5) {
	case 0:
		msg += `,"content":` + g.text()
	case 1:
		msg += `,"content":` + g.chatContentArray(func() string {
			return g.pick(
				`{"type":"text","text":`+g.text()+`}`,
				`{"type":"image_url","image_url":{"url":"data:image/png;base64,`+g.base64Data()+`"}}`,
				`{"type":"image_url","image_url":{"url":"https://example.com/y.png"}}`,
			)
		})
	case 2:
		msg += `,"content":null`
	}
	if g.chance(2) {
		var calls []string
		for n := g.r.Intn(4); n > 0; n-- {
			calls = append(calls, g.chatToolCall())
		}
		msg += `,"tool_calls":[` + strings.Join(calls, ",") + `]`
	}
	if g.chance(6) {
		msg += `,"reasoning_content":` + g.text()
	}
	return msg + `}`
}

func (g gen) chatTool() string {
	id := `,"tool_call_id":` + g.id("call_")
	if g.chance(12) {
		id = ""
	}
	return `{"role":"tool"` + id + `,"content":` + g.pick(
		g.text(),
		`"{\"ok\":true}"`,
		`"null"`,
		`""`,
		`{"result":"structured"}`,
		`[{"type":"text","text":"see image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`,
		`[{"type":"text","text":"plain list"}]`,
		`[{"type":"unknown","x":1}]`,
	) + `}`
}

func (g gen) chatMessages() []string {
	switch g.r.Intn(10) {
	case 0:
		return []string{g.chatSystem()}
	case 1, 2, 3:
		content := g.text()
		if g.chance(2) {
			content = g.chatContentArray(g.chatUserPart)
		}
		return []string{`{"role":"user","content":` + content + `}`}
	case 4, 5:
		return []string{g.chatAssistant()}
	case 6, 7:
		// A tool round: the call, then results in any order.
		calls := []string{g.chatToolCall(), g.chatToolCall()}
		msgs := []string{`{"role":"assistant","content":` + g.pick(`null`, `""`, g.text()) + `,"tool_calls":[` + strings.Join(calls[:1+g.r.Intn(2)], ",") + `]}`}
		for n := g.r.Intn(3); n > 0; n-- {
			msgs = append(msgs, g.chatTool())
		}
		return msgs
	case 8:
		return []string{g.chatTool()}
	default:
		return []string{g.pick(`{"role":"function","name":"f","content":"x"}`, `{"content":"no role"}`, `"scalar"`, `{"role":7}`)}
	}
}

func (g gen) chatTools() string {
	var tools []string
	for n := g.r.Intn(5); n > 0; n-- {
		tools = append(tools, g.pick(
			`{"type":"function","function":{"name":"shell","description":"run","parameters":`+g.schema()+`}}`,
			`{"type":"function","function":{"name":"read","parameters":`+g.schema()+`,"strict":true}}`,
			`{"type":"function","function":{"name":"bare"}}`,
			`{"type":"function","function":{"name":"bad","parameters":"not a schema"}}`,
			`{"type":"function"}`,
			`{"google_search":{}}`,
			`{"code_execution":{}}`,
			`{"url_context":{}}`,
			`{"type":"web_search"}`,
		))
	}
	return strings.Join(tools, ",")
}

// CodexChatSession is a Chat Completions request with turns tool calls, each
// followed by its result, and a screenshot every fifth turn: a long agent
// session as an OpenAI-compatible client sends it.
func CodexChatSession(turns int) []byte {
	msgs := []string{
		`{"role":"system","content":"be brief"}`,
		`{"role":"user","content":[{"type":"text","text":"fix the build"}]}`,
	}
	for i := 0; i < turns; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			`{"role":"assistant","content":"step `+id+`","tool_calls":[{"id":"`+id+`","type":"function","function":{"name":"shell","arguments":"{\"cmd\":[\"ls\",\"-la\"]}"}}]}`,
			`{"role":"tool","tool_call_id":"`+id+`","content":"`+strings.Repeat("line of tool output ", 100)+`"}`,
		)
		if i%5 == 4 {
			msgs = append(msgs, `{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,`+strings.Repeat("iVBORw0K", 2000)+`"}}]}`)
		}
	}
	return []byte(`{"model":"gpt-4o","messages":[` + strings.Join(msgs, ",") + `],"tools":[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"array"}}}}}],"stream":true}`)
}

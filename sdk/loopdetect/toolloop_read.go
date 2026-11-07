package loopdetect

import (
	"encoding/json"
	"hash/maphash"
	"io"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Protocol names the API a request is written in. Its values are the handler
// types of the entries that take the request, which is also what the executors
// translate from.
type Protocol string

const (
	ProtocolChat      Protocol = "openai"
	ProtocolResponses Protocol = "openai-response"
	ProtocolClaude    Protocol = "claude"
	ProtocolGemini    Protocol = "gemini"
)

// ProtocolOf maps an entry's handler type to the protocol it speaks. The Gemini
// CLI bridge is not one: it is a local route with no API key, so no policy ever
// reaches it.
func ProtocolOf(handlerType string) (Protocol, bool) {
	switch p := Protocol(handlerType); p {
	case ProtocolChat, ProtocolResponses, ProtocolClaude, ProtocolGemini:
		return p, true
	}
	return "", false
}

// conversationPath is where each API keeps a request's conversation.
func (p Protocol) conversationPath() string {
	switch p {
	case ProtocolChat, ProtocolClaude:
		return "messages"
	case ProtocolResponses:
		return "input"
	case ProtocolGemini:
		return "contents"
	}
	return ""
}

// resultMarks are what each API writes a tool result with, for a body with
// fewer of them than a loop needs to be let by unread.
func (p Protocol) resultMarks() []string {
	switch p {
	case ProtocolChat:
		return []string{`"tool_call_id"`}
	case ProtocolClaude:
		return []string{`"tool_result"`}
	case ProtocolResponses:
		return []string{`_output"`} // function_call_output, custom_tool_call_output, ...
	case ProtocolGemini:
		return []string{`"functionResponse"`, `"function_response"`}
	}
	return nil
}

// side is who wrote a message of the conversation.
type side uint8

const (
	sideModel side = iota + 1
	sideUser
)

type partKind uint8

const (
	// partCall: the model asked for a tool.
	partCall partKind = iota + 1
	// partResult: the client's answer to a call.
	partResult
	// partOther: anything else the client side said, a user's words or a
	// reminder an agent adds, which count as part of the turn.
	partOther
)

// part is one piece of a message, reduced to what tells turns apart. The model
// side's text and reasoning are not kept: a looping model words them
// differently every round, and only its calls and what came back matter.
type part struct {
	kind partKind
	id   string // of a call; of the call a result answers
	name string // of a tool: a call's, and a result's where the API names it
	sum  uint64 // of a call's arguments; of a result's or other part's content
	// shown is a call's arguments cut short, for the notice.
	shown string
}

type message struct {
	side  side
	parts []part
}

var hashSeed = maphash.MakeSeed()

func sumOf(s string) uint64 { return maphash.String(hashSeed, s) }

// argsShownRunes bounds the arguments a notice quotes.
const argsShownRunes = 120

// maxCanonicalBytes: arguments longer than this are compared as written. They
// are a file a tool writes, which a client writes the same way every time.
const maxCanonicalBytes = 16 << 10

// readMessages turns the newest items of a conversation into messages.
func readMessages(p Protocol, items []gjson.Result) []message {
	msgs := make([]message, 0, len(items))
	for _, item := range items {
		switch p {
		case ProtocolClaude:
			msgs = append(msgs, readClaude(item))
		case ProtocolChat:
			msgs = append(msgs, readChat(item))
		case ProtocolResponses:
			msgs = append(msgs, readResponses(item))
		case ProtocolGemini:
			msgs = append(msgs, readGemini(item))
		}
	}
	return msgs
}

func readClaude(item gjson.Result) message {
	m := message{side: sideUser}
	if item.Get("role").String() == "assistant" {
		m.side = sideModel
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		m.addText(content.Str)
		return m
	}
	content.ForEach(func(_, block gjson.Result) bool {
		switch block.Get("type").String() {
		case "text":
			m.addText(block.Get("text").String())
		case "tool_use":
			m.addCall(block.Get("id").String(), block.Get("name").String(), block.Get("input").Raw)
		case "tool_result":
			m.addResult(block.Get("tool_use_id").String(), "", contentText(block.Get("content")), block.Get("is_error").Bool())
		case "thinking", "redacted_thinking":
			// a model's reasoning, different every round
		default: // an image, a document, a server tool's block
			m.addOther(block.Raw)
		}
		return true
	})
	return m
}

func readChat(item gjson.Result) message {
	switch item.Get("role").String() {
	case "assistant":
		m := message{side: sideModel}
		item.Get("tool_calls").ForEach(func(_, call gjson.Result) bool {
			fn := call.Get("function")
			m.addCall(call.Get("id").String(), fn.Get("name").String(), fn.Get("arguments").String())
			return true
		})
		return m
	case "tool":
		m := message{side: sideUser}
		m.addResult(item.Get("tool_call_id").String(), "", contentText(item.Get("content")), false)
		return m
	default: // user, system, developer
		m := message{side: sideUser}
		m.addText(contentText(item.Get("content")))
		return m
	}
}

func readResponses(item gjson.Result) message {
	typ := item.Get("type").String()
	if typ == "" {
		typ = "message" // the short form of a message has no type
	}
	switch {
	case typ == "message":
		if item.Get("role").String() == "assistant" {
			return message{side: sideModel}
		}
		m := message{side: sideUser}
		m.addText(contentText(item.Get("content")))
		return m
	case strings.HasSuffix(typ, "_output"):
		// function_call_output, custom_tool_call_output, local_shell_call_output, ...
		m := message{side: sideUser}
		m.addResult(item.Get("call_id").String(), "", contentText(item.Get("output")), false)
		return m
	case strings.HasSuffix(typ, "_call") && item.Get("call_id").Exists():
		// function_call, custom_tool_call, local_shell_call, computer_call, ...
		name := item.Get("name").String()
		if name == "" {
			name = typ
		}
		args := ""
		for _, key := range []string{"arguments", "input", "action", "operation"} {
			if v := item.Get(key); v.Exists() {
				if v.Type == gjson.String {
					args = v.Str
				} else {
					args = v.Raw
				}
				break
			}
		}
		m := message{side: sideModel}
		m.addCall(item.Get("call_id").String(), name, args)
		return m
	default:
		// reasoning, item references and the calls a provider runs itself
		// (web_search_call, mcp_call, ...): the model's side, and no call of
		// the client's to answer
		return message{side: sideModel}
	}
}

func readGemini(item gjson.Result) message {
	m := message{side: sideUser}
	if item.Get("role").String() == "model" {
		m.side = sideModel
	}
	item.Get("parts").ForEach(func(_, p gjson.Result) bool {
		if fc := firstOf(p, "functionCall", "function_call"); fc.Exists() {
			m.addCall(fc.Get("id").String(), fc.Get("name").String(), fc.Get("args").Raw)
			return true
		}
		if fr := firstOf(p, "functionResponse", "function_response"); fr.Exists() {
			m.addResult(fr.Get("id").String(), fr.Get("name").String(), contentText(fr.Get("response")), false)
			return true
		}
		if p.Get("thought").Bool() {
			return true
		}
		if text := p.Get("text"); text.Exists() {
			m.addText(text.String())
			return true
		}
		m.addOther(p.Raw)
		return true
	})
	return m
}

func firstOf(v gjson.Result, keys ...string) gjson.Result {
	for _, key := range keys {
		if r := v.Get(key); r.Exists() {
			return r
		}
	}
	return gjson.Result{}
}

// addCall adds a call of the model's. Calls are only read from the model's side.
func (m *message) addCall(id, name, args string) {
	if m.side != sideModel {
		return
	}
	canon := args
	if len(args) <= maxCanonicalBytes {
		canon = canonicalJSON(args, nil)
	}
	m.parts = append(m.parts, part{kind: partCall, id: id, name: name, sum: sumOf(canon), shown: shorten(canon, argsShownRunes)})
}

// addResult adds a result of the client's. Results are only read from the
// client's side.
func (m *message) addResult(id, name, text string, isError bool) {
	if m.side != sideUser {
		return
	}
	sum := sumOf(steadyResult(text))
	if isError {
		sum ^= 0x9e3779b97f4a7c15 // an error and a result with the same words differ
	}
	m.parts = append(m.parts, part{kind: partResult, id: id, name: name, sum: sum})
}

// addText adds words of the client's side; the model's are not kept.
func (m *message) addText(text string) {
	if m.side != sideUser {
		return
	}
	m.parts = append(m.parts, part{kind: partOther, sum: sumOf("text\x00" + text)})
}

// addOther adds what else the client's side holds: an image, a document, a file.
func (m *message) addOther(raw string) {
	if m.side != sideUser {
		return
	}
	m.parts = append(m.parts, part{kind: partOther, sum: sumOf("other\x00" + raw)})
}

// contentText is the text of a message's content or a tool's output as one
// string: a plain string, the text blocks of a list (anything else in it as
// written), or a JSON object as written in a canonical form.
func contentText(v gjson.Result) string {
	switch {
	case v.Type == gjson.String:
		return v.Str
	case v.IsArray():
		var b strings.Builder
		v.ForEach(func(_, item gjson.Result) bool {
			if b.Len() > 0 {
				b.WriteByte('\x01')
			}
			if item.Type == gjson.String {
				b.WriteString(item.Str)
			} else if t := item.Get("text"); t.Type == gjson.String {
				b.WriteString(t.Str)
			} else {
				b.WriteString(item.Raw)
			}
			return true
		})
		return b.String()
	case v.IsObject():
		if len(v.Raw) <= maxCanonicalBytes {
			// a result's text sits in a string of the object, where a line's
			// start is no longer a line's start: steady the strings themselves
			return canonicalJSON(v.Raw, steadyResult)
		}
		return v.Raw
	case v.Exists() && v.Type != gjson.Null:
		return v.Raw
	}
	return ""
}

// canonicalJSON writes raw, a JSON value, with its object keys in one order, as
// two turns asking the same may write them differently. Text that is not JSON
// is returned as written. Every string in the value goes through steady first,
// when it is not nil.
func canonicalJSON(raw string, steady func(string) string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || (raw[0] != '{' && raw[0] != '[') {
		return raw
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	if _, err := dec.Token(); err != io.EOF {
		return raw
	}
	if steady != nil {
		v = steadyStrings(v, steady)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

func steadyStrings(v any, steady func(string) string) any {
	switch x := v.(type) {
	case string:
		return steady(x)
	case map[string]any:
		for k, e := range x {
			x[k] = steadyStrings(e, steady)
		}
	case []any:
		for i, e := range x {
			x[i] = steadyStrings(e, steady)
		}
	}
	return v
}

// shorten cuts s to n characters for a message to quote.
func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// runNoise is what an agent writes into a result about the run itself rather
// than about what it found, and which differs each time the same command is
// run: the wall time and chunk id Codex's shell prints, and a shell tool's
// duration.
var runNoise = []struct {
	mark string // in a result the expression may change
	re   *regexp.Regexp
	with string
}{
	{"Wall time", regexp.MustCompile(`(?m)^(Wall time:? )[0-9.]+( seconds?)`), "${1}#${2}"},
	{"Chunk ID: ", regexp.MustCompile(`(?m)^(Chunk ID: )\S+`), "${1}#"},
	{`"duration_seconds"`, regexp.MustCompile(`("duration_seconds"\s*:\s*)[0-9.eE+-]+`), "${1}#"},
}

func steadyResult(s string) string {
	for _, n := range runNoise {
		if strings.Contains(s, n.mark) {
			s = n.re.ReplaceAllString(s, n.with)
		}
	}
	return s
}
